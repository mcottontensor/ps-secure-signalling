package sfu

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/wsconn"
)

// Hooks let an embedder observe and control the SFU. Every field is optional. Hooks run on SFU
// goroutines and must not block for long.
type Hooks struct {
	// OnPlayerJoin runs when a player subscribes to the SFU. Returning an error refuses the player.
	OnPlayerJoin func(playerID string) error
	// OnPlayerLeave runs when a player's session ends, for whatever reason.
	OnPlayerLeave func(playerID string)
	// OnStreamerConnected runs once the streamer's media is flowing and players can join.
	OnStreamerConnected func()
}

// SFU is one forwarding unit: one upstream streamer, any number of players.
type SFU struct {
	Hooks Hooks

	cfg      Config
	log      *slog.Logger
	settings webrtc.SettingEngine
	upstream *webrtc.API
	tls      *tls.Config

	mu         sync.Mutex
	conn       *wsconn.Conn
	selfID     string
	iceServers []webrtc.ICEServer
	streamer   *streamer
	players    map[string]*player
	search     *time.Timer

	// view is an immutable snapshot of the players, read for every forwarded packet without
	// taking mu.
	view atomic.Pointer[[]*player]
}

// New builds an SFU. cfg should come from LoadConfig, or have Validate called on it.
func New(cfg Config, log *slog.Logger) (*SFU, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	se, err := settingEngine(cfg.ICE)
	if err != nil {
		return nil, err
	}
	api, err := streamerAPI(se, cfg.Codecs)
	if err != nil {
		return nil, err
	}
	s := &SFU{cfg: cfg, log: log, settings: se, upstream: api, players: map[string]*player{}, selfID: cfg.SFUID}
	if strings.HasPrefix(cfg.SignallingURL, "wss://") {
		if s.tls, err = cfg.TLS.Client(); err != nil {
			return nil, err
		}
		if cfg.TLS.InsecureSkipVerify {
			log.Warn("TLS certificate verification of the signalling server is DISABLED; never do this in production")
		}
	}
	s.view.Store(&[]*player{})
	return s, nil
}

// Run connects to the signalling server and serves until ctx ends, reconnecting with backoff.
func (s *SFU) Run(ctx context.Context) error {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := s.session(ctx)
		s.teardownStreamer(false)
		if ctx.Err() != nil {
			break
		}
		if time.Since(began) > time.Minute {
			backoff = 2 * time.Second
		}
		s.log.Warn("signalling connection ended; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	s.teardownStreamer(false)
	return ctx.Err()
}

func (s *SFU) session(ctx context.Context) error {
	header := http.Header{}
	if s.cfg.Token != "" {
		header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: s.tls, Proxy: http.ProxyFromEnvironment},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, resp, err := websocket.Dial(dctx, s.cfg.SignallingURL, &websocket.DialOptions{HTTPHeader: header, HTTPClient: client})
	cancel()
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("signalling server refused our credentials: %w", err)
		}
		return err
	}
	conn := wsconn.New(ws, wsconn.Options{MaxMessageBytes: 1 << 20})
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	s.log.Info("connected to signalling server", "url", s.cfg.SignallingURL)

	conn.Run(ctx, func(m protocol.Message) {
		defer s.recover("signalling message " + protocol.TypeOf(m))
		s.handle(m)
	}, func(err error) { s.log.Warn("invalid signalling message", "err", err) })

	s.mu.Lock()
	s.conn = nil
	if s.search != nil {
		s.search.Stop()
		s.search = nil
	}
	s.mu.Unlock()
	return errors.New("disconnected")
}

// send queues a message to the signalling server, if connected.
func (s *SFU) send(m protocol.Message) {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c != nil {
		c.Send(m)
	}
}

func (s *SFU) recover(what string) {
	if r := recover(); r != nil {
		s.log.Error("recovered from panic", "in", what, "panic", r, "stack", string(debug.Stack()))
	}
}

func (s *SFU) handle(m protocol.Message) {
	switch v := m.(type) {
	case *protocol.Config:
		s.setICEServers(v.PeerConnectionOptions)
	case *protocol.Identify:
		s.send(&protocol.EndpointID{ID: s.cfg.SFUID, ProtocolVersion: protocol.Version})
		s.send(&protocol.ListStreamers{})
	case *protocol.EndpointIDConfirm:
		s.mu.Lock()
		s.selfID = v.CommittedID
		s.mu.Unlock()
		s.log.Info("registered with signalling server", "id", v.CommittedID)
	case *protocol.StreamerList:
		s.onStreamerList(v.IDs)
	case *protocol.SubscribeFailed:
		s.log.Warn("subscribe failed", "reason", v.Message)
		s.scheduleSearch()
	case *protocol.Offer:
		// Always the upstream streamer's: players never send offers to an SFU. Its playerId, if
		// any, is our own id as a player, left in place by older signalling servers.
		s.onStreamerOffer(v)
	case *protocol.Answer:
		if p := s.player(string(v.PlayerID)); p != nil {
			p.setAnswer(v.SDP)
		}
	case *protocol.IceCandidate:
		if p := s.player(string(v.PlayerID)); p != nil {
			p.addCandidate(v.Candidate)
		} else if st := s.currentStreamer(); st != nil {
			st.addCandidate(v.Candidate)
		}
	case *protocol.PlayerConnected:
		s.onPlayerConnected(string(v.PlayerID))
	case *protocol.PlayerDisconnected:
		s.removePlayer(string(v.PlayerID), "player disconnected")
	case *protocol.StreamerDisconnected:
		s.log.Info("streamer disconnected")
		s.teardownStreamer(true)
		s.scheduleSearch()
	case *protocol.DataChannelRequest:
		if p := s.player(string(v.PlayerID)); p != nil {
			p.onDataChannelRequest()
		}
	case *protocol.PeerDataChannelsReady:
		if p := s.player(string(v.PlayerID)); p != nil {
			p.onDataChannelsReady()
		}
	case *protocol.LayerPreference:
		if p := s.player(string(v.PlayerID)); p != nil {
			p.setCeiling(int(v.SpatialLayer))
		}
	case *protocol.StreamerIDChanged:
		s.log.Info("streamer changed id", "id", v.NewID)
	case *protocol.Ping:
		s.send(&protocol.Pong{Time: v.Time})
	default:
		s.log.Debug("ignoring signalling message", "type", protocol.TypeOf(m))
	}
}

// setICEServers takes the iceServers from the config message's peer connection options. They may
// carry per-connection TURN credentials.
func (s *SFU) setICEServers(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var opts struct {
		IceServers []struct {
			URLs       json.RawMessage `json:"urls"`
			URL        string          `json:"url"`
			Username   string          `json:"username"`
			Credential string          `json:"credential"`
		} `json:"iceServers"`
	}
	if err := json.Unmarshal(raw, &opts); err != nil {
		s.log.Warn("ignoring unreadable peerConnectionOptions", "err", err)
		return
	}
	var servers []webrtc.ICEServer
	for _, e := range opts.IceServers {
		var urls []string
		if json.Unmarshal(e.URLs, &urls) != nil {
			var one string
			if json.Unmarshal(e.URLs, &one) == nil && one != "" {
				urls = []string{one}
			}
		}
		if e.URL != "" {
			urls = append(urls, e.URL)
		}
		if len(urls) > 0 {
			servers = append(servers, webrtc.ICEServer{URLs: urls, Username: e.Username, Credential: e.Credential})
		}
	}
	s.mu.Lock()
	s.iceServers = servers
	s.mu.Unlock()
}

func (s *SFU) rtcConfig() webrtc.Configuration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return webrtc.Configuration{ICEServers: slices.Clone(s.iceServers), BundlePolicy: webrtc.BundlePolicyMaxBundle}
}

func (s *SFU) onStreamerList(ids []string) {
	s.mu.Lock()
	busy, self := s.streamer != nil, s.selfID
	s.mu.Unlock()
	if busy {
		return
	}
	ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == self || id == s.cfg.SFUID })
	target := ""
	switch {
	case s.cfg.SubscribeStreamerID != "":
		if slices.Contains(ids, s.cfg.SubscribeStreamerID) {
			target = s.cfg.SubscribeStreamerID
		}
	case len(ids) > 0:
		target = ids[0]
	}
	if target == "" {
		s.log.Info("no suitable streamer yet", "want", s.cfg.SubscribeStreamerID, "available", ids)
		s.scheduleSearch()
		return
	}
	s.log.Info("subscribing to streamer", "id", target)
	s.send(&protocol.Subscribe{StreamerID: target})
}

// scheduleSearch asks for the streamer list again after a delay. Only one search is ever pending.
func (s *SFU) scheduleSearch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.search != nil {
		s.search.Stop()
	}
	s.search = time.AfterFunc(s.cfg.RetrySubscribeDelay, func() { s.send(&protocol.ListStreamers{}) })
}

func (s *SFU) currentStreamer() *streamer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamer
}

func (s *SFU) onStreamerOffer(o *protocol.Offer) {
	s.mu.Lock()
	if s.streamer != nil {
		c := s.conn
		s.mu.Unlock()
		// As the old SFU did: start again from a clean slate on every side rather than trying to
		// reconcile a second producer with players already bound to the first.
		s.log.Warn("streamer offered again while already connected; resetting signalling connection")
		if c != nil {
			c.Close(websocket.StatusTryAgainLater, "producer is already connected")
		}
		return
	}
	s.mu.Unlock()

	st, err := newStreamer(s, o)
	if err != nil {
		s.log.Error("could not accept streamer offer", "err", err)
		return
	}
	s.mu.Lock()
	s.streamer = st
	s.mu.Unlock()
	go st.answer()
}

// teardownStreamer drops every player and the streamer. With notify it tells the signalling
// server we stopped streaming. Safe to call when already disconnected.
func (s *SFU) teardownStreamer(notify bool) {
	s.mu.Lock()
	st, players := s.streamer, s.players
	s.streamer, s.players = nil, map[string]*player{}
	s.view.Store(&[]*player{})
	s.mu.Unlock()
	for _, p := range players {
		p.close()
	}
	if st != nil {
		st.close()
		if notify {
			s.send(&protocol.StopStreaming{})
		}
	}
}

// streamerFailed handles the streamer's peer connection dying without the signalling server
// noticing: give up on it, and look for a streamer again.
func (s *SFU) streamerFailed(st *streamer) {
	if s.currentStreamer() != st {
		return
	}
	s.log.Warn("streamer connection failed")
	s.teardownStreamer(true)
	s.send(&protocol.Unsubscribe{})
	s.scheduleSearch()
}

func (s *SFU) player(id string) *player {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.players[id]
}

// refreshView republishes the players that receive media. Call with mu held.
func (s *SFU) refreshView() {
	v := make([]*player, 0, len(s.players))
	for _, p := range s.players {
		if p.active {
			v = append(v, p)
		}
	}
	s.view.Store(&v)
}

// activate starts forwarding media to a player whose tracks are set up.
func (s *SFU) activate(p *player) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.players[p.id] == p {
		p.active = true
		s.refreshView()
	}
}

func (s *SFU) onPlayerConnected(id string) {
	refuse := func(reason string) {
		s.log.Warn("refusing player", "player", id, "reason", reason)
		s.send(&protocol.DisconnectPlayer{PlayerID: protocol.PlayerID(id), Reason: reason})
	}
	if id == "" {
		return
	}
	s.mu.Lock()
	st := s.streamer
	_, exists := s.players[id]
	full := s.cfg.MaxPlayers > 0 && len(s.players) >= s.cfg.MaxPlayers
	s.mu.Unlock()
	switch {
	case st == nil:
		s.log.Info("no streamer connected, ignoring player", "player", id)
		return
	case exists:
		s.removePlayer(id, "player reconnected")
	case full:
		refuse("SFU is full")
		return
	}
	if h := s.Hooks.OnPlayerJoin; h != nil {
		if err := h(id); err != nil {
			refuse(err.Error())
			return
		}
	}
	p := newPlayer(s, st, id)
	s.mu.Lock()
	if s.streamer != st { // the streamer went away meanwhile
		s.mu.Unlock()
		return
	}
	s.players[id] = p
	s.refreshView()
	s.mu.Unlock()
	go p.start()
}

func (s *SFU) removePlayer(id, why string) {
	s.mu.Lock()
	p := s.players[id]
	delete(s.players, id)
	s.refreshView()
	s.mu.Unlock()
	if p != nil {
		s.log.Info("player left", "player", id, "reason", why)
		p.close()
	}
}
