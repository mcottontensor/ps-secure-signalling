// Package signalling is a Pixel Streaming signalling server: it authenticates streamers, players and
// SFUs, keeps track of who is subscribed to whom, and relays the WebRTC negotiation between them.
//
// It is an http.Handler factory rather than a process: mount Handler(role) wherever suits, behind
// whatever middleware suits. cmd/wilbur is one way to assemble it.
package signalling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/wsconn"
)

// Config configures a Server. The zero value is usable, if permissive about counts.
type Config struct {
	// PeerOptions is sent to every peer as peerConnectionOptions (ICE servers and so on), unless a
	// PeerOptions hook replaces it.
	PeerOptions json.RawMessage

	MaxSubscribers int // per streamer; 0 means unlimited
	MaxPlayers     int // total player connections; 0 means unlimited
	MaxStreamers   int // total streamer and SFU connections; 0 means unlimited
	MaxConnsPerIP  int // concurrent connections from one address, all roles; 0 means unlimited

	// Conn sets per-role connection limits. Roles without an entry get DefaultConnOptions.
	Conn map[auth.Role]wsconn.Options

	// AllowedOrigins are extra Origin host patterns (path.Match syntax, e.g. "*.example.com")
	// accepted on websocket upgrades. Requests from the same host as the server are always
	// accepted, as are requests with no Origin (which browsers always send, so these are not
	// browsers). This is what stops another website opening connections with a visitor's cookies.
	AllowedOrigins []string

	// LegacyAutoSubscribe subscribes a player that sends negotiation messages without subscribing
	// to the first streamer it is allowed to see, as Wilbur does for very old clients.
	LegacyAutoSubscribe bool

	// CloseOnExpiry closes connections when the credential they were opened with expires.
	CloseOnExpiry bool

	// ClientIP extracts the client address used for per-IP limits and logs. Defaults to the
	// connection's remote address; replace it when running behind a trusted proxy.
	ClientIP func(*http.Request) string

	Logger *slog.Logger
}

// DefaultConnOptions are the per-role connection limits used when Config.Conn has no entry.
// Streamers and SFUs relay negotiation for many players, so get a higher message rate.
var DefaultConnOptions = map[auth.Role]wsconn.Options{
	auth.RolePlayer:   {MessagesPerSecond: 50, Burst: 200},
	auth.RoleStreamer: {MessagesPerSecond: 500, Burst: 2000, SendQueue: 1024},
	auth.RoleSFU:      {MessagesPerSecond: 500, Burst: 2000, SendQueue: 1024},
}

// Server is the signalling server. All routing state is guarded by one mutex: signalling traffic is
// light, every send is a non-blocking enqueue, and one lock is easy to reason about.
type Server struct {
	cfg   Config
	authn auth.Authenticator
	hooks Hooks
	log   *slog.Logger
	start time.Time

	mu         sync.Mutex
	streamers  map[string]*peer // streamers and SFUs, by streamer id
	players    map[string]*peer // players and SFUs, by player id
	perIP      map[string]int
	nextPlayer uint64
	nextTemp   uint64
	closed     bool
}

// New creates a server. authn decides who may connect in which role; see auth.Chain.
func New(cfg Config, authn auth.Authenticator, hooks Hooks) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ClientIP == nil {
		cfg.ClientIP = RemoteHost
	}
	if len(cfg.PeerOptions) == 0 {
		cfg.PeerOptions = json.RawMessage(`{}`)
	}
	return &Server{
		cfg:       cfg,
		authn:     authn,
		hooks:     hooks,
		log:       cfg.Logger,
		start:     time.Now(),
		streamers: map[string]*peer{},
		players:   map[string]*peer{},
		perIP:     map[string]int{},
	}
}

// RemoteHost returns the host part of the request's remote address.
func RemoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Handler returns the websocket endpoint for one role. Mount each on its own listener: keeping the
// streamer and SFU endpoints off the public interface is the first layer of defence.
func (s *Server) Handler(role auth.Role) http.Handler {
	switch role {
	case auth.RolePlayer, auth.RoleStreamer, auth.RoleSFU:
	default:
		panic("signalling: no websocket endpoint for role " + string(role))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, role) })
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, role auth.Role) {
	ip := s.cfg.ClientIP(r)
	log := s.log.With("role", role, "remote", ip)

	id, err := s.authn.Authenticate(r.Context(), r, role)
	if err != nil {
		log.Info("connection refused", "reason", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	log = log.With("subject", id.Subject, "auth", id.Method)

	if s.hooks.OnConnect != nil {
		if err := s.hooks.OnConnect(r, PeerInfo{Role: role, Identity: id, RemoteAddr: ip}); err != nil {
			log.Info("connection refused by hook", "reason", err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	if err := s.admit(role, ip); err != nil {
		log.Warn("connection refused", "reason", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.release(ip)

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  s.cfg.AllowedOrigins,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		log.Info("websocket upgrade failed", "err", err)
		return
	}
	opts, ok := s.cfg.Conn[role]
	if !ok {
		opts = DefaultConnOptions[role]
	}
	p := &peer{role: role, identity: id, remote: ip, conn: wsconn.New(ws, opts), subscribers: map[string]*peer{}}

	if !s.register(p) {
		p.conn.Close(websocket.StatusGoingAway, "server shutting down")
		p.conn.Run(context.Background(), func(protocol.Message) {}, nil)
		return
	}
	info := s.info(p)
	log = log.With("player", info.PlayerID, "streamer", info.StreamerID)
	log.Info("connected")
	if s.hooks.OnRegistered != nil {
		s.hooks.OnRegistered(info)
	}

	if s.cfg.CloseOnExpiry && !id.ExpiresAt.IsZero() {
		t := time.AfterFunc(time.Until(id.ExpiresAt), func() {
			log.Info("credential expired, closing")
			p.conn.Close(wsconn.StatusExpired, "credential expired")
		})
		defer t.Stop()
	}

	// The request context ends when the handler returns, which is when Run returns, so the
	// connection is not tied to anything shorter-lived.
	p.conn.Run(context.WithoutCancel(r.Context()),
		func(m protocol.Message) { s.dispatch(p, m) },
		func(err error) { log.Debug("invalid message", "err", err) })

	info = s.unregister(p)
	log.Info("disconnected")
	if s.hooks.OnDisconnect != nil {
		s.hooks.OnDisconnect(info)
	}
}

var errFull = errors.New("server full")

func (s *Server) admit(role auth.Role, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.MaxConnsPerIP > 0 && s.perIP[ip] >= s.cfg.MaxConnsPerIP {
		return fmt.Errorf("%w: too many connections from this address", errFull)
	}
	switch role {
	case auth.RolePlayer:
		if s.cfg.MaxPlayers > 0 && s.countRole(s.players, auth.RolePlayer) >= s.cfg.MaxPlayers {
			return fmt.Errorf("%w: player limit reached", errFull)
		}
	default:
		if s.cfg.MaxStreamers > 0 && len(s.streamers) >= s.cfg.MaxStreamers {
			return fmt.Errorf("%w: streamer limit reached", errFull)
		}
	}
	s.perIP[ip]++
	return nil
}

func (s *Server) release(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perIP[ip]--; s.perIP[ip] <= 0 {
		delete(s.perIP, ip)
	}
}

func (s *Server) countRole(m map[string]*peer, role auth.Role) int {
	n := 0
	for _, p := range m {
		if p.role == role {
			n++
		}
	}
	return n
}

// register assigns ids and sends the greeting. Streamers start under a placeholder id, which they
// replace with endpointId in reply to identify.
func (s *Server) register(p *peer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if p.isPlayer() {
		p.playerID = fmt.Sprintf("Player%d", s.nextPlayer)
		s.nextPlayer++
		s.players[p.playerID] = p
	}
	if p.isStreamer() {
		p.streamerID = s.uniqueStreamerID(defaultStreamerID)
		s.streamers[p.streamerID] = p
		p.conn.Send(&protocol.Identify{})
	}
	p.conn.Send(&protocol.Config{
		PeerConnectionOptions: s.peerOptions(p),
		ProtocolVersion:       protocol.Version,
	})
	return true
}

func (s *Server) peerOptions(p *peer) json.RawMessage {
	if s.hooks.PeerOptions != nil {
		// Called under the lock, unlike other hooks, so that it sees the ids just assigned. It
		// must not call back into the Server.
		if opts := s.hooks.PeerOptions(p.infoLocked()); len(opts) > 0 {
			return opts
		}
	}
	return s.cfg.PeerOptions
}

// unregister undoes everything the connection was part of.
func (s *Server) unregister(p *peer) PeerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := p.infoLocked()
	if p.isPlayer() {
		s.unsubscribeLocked(p)
		delete(s.players, p.playerID)
	}
	if p.isStreamer() && s.streamers[p.streamerID] == p {
		delete(s.streamers, p.streamerID)
		s.dropSubscribersLocked(p, "streamer disconnected")
	}
	return info
}

func (s *Server) info(p *peer) PeerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return p.infoLocked()
}

// Snapshot describes the server's current state, for the REST API.
type Snapshot struct {
	Uptime    time.Duration  `json:"-"`
	Streamers []StreamerInfo `json:"streamers"`
	Players   []PlayerInfo   `json:"players"`
}

type StreamerInfo struct {
	PeerInfo
	Streaming   bool     `json:"streaming"`
	Subscribers []string `json:"subscribers"`
}

type PlayerInfo struct {
	PeerInfo
	SubscribedTo string `json:"subscribedTo,omitempty"`
}

// Snapshot returns the current streamers and players.
func (s *Server) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{Uptime: time.Since(s.start), Streamers: []StreamerInfo{}, Players: []PlayerInfo{}}
	for _, p := range s.streamers {
		si := StreamerInfo{PeerInfo: p.infoLocked(), Streaming: p.streaming, Subscribers: []string{}}
		for id := range p.subscribers {
			si.Subscribers = append(si.Subscribers, id)
		}
		snap.Streamers = append(snap.Streamers, si)
	}
	for _, p := range s.players {
		pi := PlayerInfo{PeerInfo: p.infoLocked()}
		if p.upstream != nil {
			pi.SubscribedTo = p.upstream.streamerID
		}
		snap.Players = append(snap.Players, pi)
	}
	return snap
}

// Close disconnects everyone and refuses new connections.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, p := range s.streamers {
		p.conn.Close(websocket.StatusGoingAway, "server shutting down")
	}
	for _, p := range s.players {
		p.conn.Close(websocket.StatusGoingAway, "server shutting down")
	}
}
