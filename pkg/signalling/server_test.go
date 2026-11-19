package signalling_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/signalling"
)

// harness runs a Server with one httptest listener per role.
type harness struct {
	t                     *testing.T
	srv                   *signalling.Server
	player, streamer, sfu *httptest.Server
}

func newHarness(t *testing.T, cfg signalling.Config, authn auth.Authenticator, hooks signalling.Hooks) *harness {
	t.Helper()
	if authn == nil {
		authn = &auth.Chain{Anonymous: []auth.Role{auth.RolePlayer, auth.RoleStreamer, auth.RoleSFU}}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv := signalling.New(cfg, authn, hooks)
	h := &harness{
		t:        t,
		srv:      srv,
		player:   httptest.NewServer(srv.Handler(auth.RolePlayer)),
		streamer: httptest.NewServer(srv.Handler(auth.RoleStreamer)),
		sfu:      httptest.NewServer(srv.Handler(auth.RoleSFU)),
	}
	t.Cleanup(func() {
		srv.Close()
		h.player.Close()
		h.streamer.Close()
		h.sfu.Close()
	})
	return h
}

// client is a raw protocol peer. A background reader feeds msgs, because coder/websocket closes a
// connection whose Read context expires, so reads cannot simply time out.
type client struct {
	t      *testing.T
	ws     *websocket.Conn
	msgs   chan []byte
	closed chan websocket.StatusCode
}

func dial(t *testing.T, srv *httptest.Server, header http.Header) (*client, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, resp, err
	}
	ws.SetReadLimit(1 << 20)
	c := &client{t: t, ws: ws, msgs: make(chan []byte, 64), closed: make(chan websocket.StatusCode, 1)}
	go func() {
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				c.closed <- websocket.CloseStatus(err)
				return
			}
			c.msgs <- data
		}
	}()
	t.Cleanup(func() { ws.CloseNow() })
	return c, resp, nil
}

func mustDial(t *testing.T, srv *httptest.Server, header http.Header) *client {
	t.Helper()
	c, _, err := dial(t, srv, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func (c *client) send(v any) {
	c.t.Helper()
	var data []byte
	if m, ok := v.(protocol.Message); ok {
		data, _ = protocol.Encode(m)
	} else {
		data, _ = json.Marshal(v)
	}
	if err := c.ws.Write(context.Background(), websocket.MessageText, data); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// recv returns the next message as a generic map, so tests see exactly what is on the wire.
func (c *client) recv() map[string]any {
	c.t.Helper()
	select {
	case data := <-c.msgs:
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			c.t.Fatalf("decode %s: %v", data, err)
		}
		return m
	case code := <-c.closed:
		c.t.Fatalf("connection closed (%v) while waiting for a message", code)
	case <-time.After(3 * time.Second):
		c.t.Fatal("timed out waiting for a message")
	}
	return nil
}

// expect reads until a message of the given type arrives, failing on timeout.
func (c *client) expect(typ string) map[string]any {
	c.t.Helper()
	for {
		m := c.recv()
		if m["type"] == typ {
			return m
		}
	}
}

// expectNothing asserts no message arrives within a short window.
func (c *client) expectNothing() {
	c.t.Helper()
	select {
	case data := <-c.msgs:
		c.t.Fatalf("unexpected message: %s", data)
	case <-time.After(200 * time.Millisecond):
	}
}

// expectClosed waits for the server to close the connection and returns the close code.
func (c *client) expectClosed() websocket.StatusCode {
	c.t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case <-c.msgs:
		case code := <-c.closed:
			return code
		case <-timeout:
			c.t.Fatal("timed out waiting for close")
		}
	}
}

// connectStreamer connects a streamer and identifies it as id, returning the committed id.
func (h *harness) connectStreamer(id string, header http.Header) (*client, string) {
	h.t.Helper()
	s := mustDial(h.t, h.streamer, header)
	s.expect(protocol.TypeIdentify)
	s.expect(protocol.TypeConfig)
	s.send(&protocol.EndpointID{ID: id})
	return s, s.expect(protocol.TypeEndpointIDConfirm)["committedId"].(string)
}

// connectPlayer connects a player and subscribes it, returning its player id as the streamer saw it.
func (h *harness) connectPlayer(streamer *client, streamerID string) (*client, string) {
	h.t.Helper()
	p := mustDial(h.t, h.player, nil)
	p.expect(protocol.TypeConfig)
	p.send(&protocol.Subscribe{StreamerID: streamerID})
	return p, streamer.expect(protocol.TypePlayerConnected)["playerId"].(string)
}

func TestNegotiationFlow(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	s, id := h.connectStreamer("Game", nil)
	if id != "Game" {
		t.Fatalf("committed id %q", id)
	}
	p, pid := h.connectPlayer(s, "Game")

	s.send(&protocol.Offer{SDP: "v=0 offer", PlayerID: protocol.PlayerID(pid)})
	offer := p.expect(protocol.TypeOffer)
	if offer["sdp"] != "v=0 offer" || offer["playerId"] != nil {
		t.Fatalf("player got %v", offer)
	}
	// A player cannot choose the id its messages carry.
	p.send(map[string]any{"type": "answer", "sdp": "v=0 answer", "playerId": "Player999"})
	if ans := s.expect(protocol.TypeAnswer); ans["playerId"] != pid || ans["sdp"] != "v=0 answer" {
		t.Fatalf("streamer got %v", ans)
	}
	// Fields outside the protocol are not relayed.
	p.send(map[string]any{"type": "iceCandidate", "candidate": map[string]any{"candidate": "c", "sdpMid": "0", "sdpMLineIndex": 0}, "evil": "x"})
	if ice := s.expect(protocol.TypeIceCandidate); ice["evil"] != nil {
		t.Fatalf("unknown field relayed: %v", ice)
	}
	// Browsers send millisecond timestamps, which must be echoed intact.
	p.send(map[string]any{"type": "ping", "time": 1763452800123})
	if pong := p.expect(protocol.TypePong); pong["time"] != float64(1763452800123) {
		t.Fatalf("pong %v", pong)
	}
	p.ws.Close(websocket.StatusNormalClosure, "")
	if m := s.expect(protocol.TypePlayerDisconnected); m["playerId"] != pid {
		t.Fatalf("got %v", m)
	}
}

func TestStreamerIDCollisionIsSuffixed(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	_, a := h.connectStreamer("Game", nil)
	_, b := h.connectStreamer("Game", nil)
	if a != "Game" || b != "Game1" {
		t.Fatalf("ids %q %q", a, b)
	}
}

func TestStreamersCannotReachOtherStreamersPlayers(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	sA, _ := h.connectStreamer("A", nil)
	sB, _ := h.connectStreamer("B", nil)
	pA, pidA := h.connectPlayer(sA, "A")

	sB.send(&protocol.Offer{SDP: "hijack", PlayerID: protocol.PlayerID(pidA)})
	sB.send(&protocol.DisconnectPlayer{PlayerID: protocol.PlayerID(pidA)})
	pA.expectNothing()

	sA.send(&protocol.DisconnectPlayer{PlayerID: protocol.PlayerID(pidA), Reason: "bye"})
	if code := pA.expectClosed(); code != websocket.StatusInternalError {
		t.Fatalf("close code %v", code)
	}
}

func TestStreamerDisconnectClosesPlayers(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	s, _ := h.connectStreamer("Game", nil)
	p, _ := h.connectPlayer(s, "Game")
	s.ws.Close(websocket.StatusNormalClosure, "")
	p.expectClosed()
}

func TestSubscribeFailures(t *testing.T) {
	h := newHarness(t, signalling.Config{MaxSubscribers: 1}, nil, signalling.Hooks{})
	s, _ := h.connectStreamer("Game", nil)
	h.connectPlayer(s, "Game")

	p := mustDial(t, h.player, nil)
	p.expect(protocol.TypeConfig)
	p.send(&protocol.Subscribe{StreamerID: "Nope"})
	if m := p.expect(protocol.TypeSubscribeFailed); !strings.Contains(m["message"].(string), "does not exist") {
		t.Fatalf("got %v", m)
	}
	p.send(&protocol.Subscribe{StreamerID: "Game"})
	if m := p.expect(protocol.TypeSubscribeFailed); !strings.Contains(m["message"].(string), "full") {
		t.Fatalf("got %v", m)
	}
}

func TestListOnlyShowsIdentifiedStreamers(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	h.connectStreamer("Ready", nil)
	pending := mustDial(t, h.streamer, nil) // connected but not yet identified
	pending.expect(protocol.TypeIdentify)

	p := mustDial(t, h.player, nil)
	p.expect(protocol.TypeConfig)
	p.send(&protocol.ListStreamers{})
	ids := p.expect(protocol.TypeStreamerList)["ids"].([]any)
	if len(ids) != 1 || ids[0] != "Ready" {
		t.Fatalf("ids %v", ids)
	}
}

func TestSFURouting(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	s, _ := h.connectStreamer("Game", nil)

	sfu := mustDial(t, h.sfu, nil)
	sfu.expect(protocol.TypeIdentify)
	sfu.expect(protocol.TypeConfig)
	sfu.send(&protocol.EndpointID{ID: "SFU"})
	sfu.expect(protocol.TypeEndpointIDConfirm)
	sfu.send(&protocol.Subscribe{StreamerID: "Game"})
	connected := s.expect(protocol.TypePlayerConnected)
	if connected["sfu"] != true {
		t.Fatalf("got %v", connected)
	}
	sfuPID := connected["playerId"].(string)

	// Streamer to SFU, and back.
	s.send(&protocol.Offer{SDP: "streamer offer", PlayerID: protocol.PlayerID(sfuPID)})
	sfu.expect(protocol.TypeOffer)
	sfu.send(&protocol.Answer{SDP: "sfu answer"})
	if m := s.expect(protocol.TypeAnswer); m["playerId"] != sfuPID {
		t.Fatalf("got %v", m)
	}
	sfu.send(&protocol.StartStreaming{})

	// A player of the SFU.
	p, _ := h.connectPlayer(sfu, "SFU")
	sfu.send(&protocol.Offer{SDP: "sfu offer", PlayerID: "Player1"})
	p.expect(protocol.TypeOffer)
	p.send(&protocol.DataChannelRequest{})
	sfu.expect(protocol.TypeDataChannelRequest)
	sfu.send(&protocol.PeerDataChannels{PlayerID: "Player1", SendStreamID: 2, RecvStreamID: 3})
	if m := p.expect(protocol.TypePeerDataChannels); m["sendStreamId"] != float64(2) {
		t.Fatalf("got %v", m)
	}
	sfu.send(&protocol.StreamerDataChannels{PlayerID: "Player1", SFUID: "forged", SendStreamID: 4, RecvStreamID: 4})
	if m := s.expect(protocol.TypeStreamerDataChannels); m["sfuId"] != sfuPID || m["playerId"] != "Player1" {
		t.Fatalf("got %v", m)
	}

	// ICE candidates go to whichever side they name.
	cand := protocol.IceCandidateData{Candidate: "c", SdpMid: "0"}
	sfu.send(&protocol.IceCandidate{Candidate: cand, PlayerID: "Player1"})
	p.expect(protocol.TypeIceCandidate)
	sfu.send(&protocol.IceCandidate{Candidate: cand})
	if m := s.expect(protocol.TypeIceCandidate); m["playerId"] != sfuPID {
		t.Fatalf("got %v", m)
	}

	// The streamer leaving tells the SFU; the SFU stopping drops its players.
	s.ws.Close(websocket.StatusNormalClosure, "")
	sfu.expect(protocol.TypeStreamerDisconnected)
	sfu.send(&protocol.StopStreaming{})
	p.expectClosed()
}

func TestMessageLimits(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	p := mustDial(t, h.player, nil)
	p.expect(protocol.TypeConfig)
	big := strings.Repeat("x", 300<<10)
	_ = p.ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"offer","sdp":"`+big+`"}`))
	if code := p.expectClosed(); code != websocket.StatusMessageTooBig {
		t.Fatalf("close code %v", code)
	}

	p = mustDial(t, h.player, nil)
	p.expect(protocol.TypeConfig)
	for range 25 {
		_ = p.ws.Write(context.Background(), websocket.MessageText, []byte(`not json`))
	}
	if code := p.expectClosed(); code != websocket.StatusPolicyViolation {
		t.Fatalf("close code %v", code)
	}
}

func TestHooks(t *testing.T) {
	var disconnected = make(chan signalling.PeerInfo, 4)
	h := newHarness(t, signalling.Config{PeerOptions: json.RawMessage(`{"static":true}`)}, nil, signalling.Hooks{
		AuthorizeSubscribe: func(info signalling.PeerInfo, id string) error {
			if id == "tenant-Private" {
				return io.EOF
			}
			return nil
		},
		AuthorizeStreamerID: func(info signalling.PeerInfo, requested string) (string, error) {
			return "tenant-" + requested, nil
		},
		PeerOptions: func(info signalling.PeerInfo) json.RawMessage {
			return json.RawMessage(`{"for":"` + info.PlayerID + `"}`)
		},
		OnMessage: func(info signalling.PeerInfo, m protocol.Message) bool {
			_, isStats := m.(*protocol.Stats)
			return !isStats
		},
		OnDisconnect: func(info signalling.PeerInfo) { disconnected <- info },
	})
	_, id := h.connectStreamer("Private", nil)
	if id != "tenant-Private" {
		t.Fatalf("id %q", id)
	}
	p := mustDial(t, h.player, nil)
	if m := p.expect(protocol.TypeConfig); m["peerConnectionOptions"].(map[string]any)["for"] == "" {
		t.Fatalf("got %v", m)
	}
	p.send(&protocol.Subscribe{StreamerID: "tenant-Private"})
	p.expect(protocol.TypeSubscribeFailed)
	p.ws.Close(websocket.StatusNormalClosure, "")
	select {
	case info := <-disconnected:
		if info.Role != auth.RolePlayer {
			t.Fatalf("got %v", info)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnDisconnect not called")
	}
}

func TestPlayerCannotOfferToSFU(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	sfu := mustDial(t, h.sfu, nil)
	sfu.expect(protocol.TypeIdentify)
	sfu.send(&protocol.EndpointID{ID: "SFU"})
	sfu.expect(protocol.TypeEndpointIDConfirm)
	p, _ := h.connectPlayer(sfu, "SFU")
	p.send(&protocol.Offer{SDP: "not a streamer"})
	p.send(&protocol.Answer{SDP: "fine"})
	if m := sfu.expect(protocol.TypeAnswer); m["sdp"] != "fine" {
		t.Fatalf("got %v", m) // the offer before it was dropped
	}
	// To a real streamer, a player's offer still goes through (OfferToReceive).
	s, _ := h.connectStreamer("Game", nil)
	p2, _ := h.connectPlayer(s, "Game")
	p2.send(&protocol.Offer{SDP: "player offer"})
	if m := s.expect(protocol.TypeOffer); m["sdp"] != "player offer" {
		t.Fatalf("got %v", m)
	}
}

func TestOriginChecks(t *testing.T) {
	h := newHarness(t, signalling.Config{}, nil, signalling.Hooks{})
	origin := func(o string) http.Header { return http.Header{"Origin": {o}} }

	// Unreal (libwebsockets) sends its own address without the port.
	mustDial(t, h.streamer, origin("http://127.0.0.1")).expect(protocol.TypeIdentify)
	mustDial(t, h.sfu, origin("http://127.0.0.1")).expect(protocol.TypeIdentify)
	// Pages on other sites must never reach the streamer or SFU ports...
	for _, srv := range []*httptest.Server{h.streamer, h.sfu, h.player} {
		if _, resp, err := dial(t, srv, origin("https://evil.example")); err == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: cross-site origin accepted (%v)", srv.URL, err)
		}
	}
	// ...and players keep the strict host:port check.
	if _, _, err := dial(t, h.player, origin("http://127.0.0.1")); err == nil {
		t.Fatal("player origin without the port accepted")
	}
	mustDial(t, h.player, origin(h.player.URL)).expect(protocol.TypeConfig)
}
