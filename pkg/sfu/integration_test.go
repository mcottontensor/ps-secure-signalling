package sfu

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/wsconn"
)

// relay is just enough of a signalling server for one SFU: the test plays the streamer and the
// player itself, exchanging messages with the SFU through it.
type relay struct {
	srv  *httptest.Server
	in   chan protocol.Message
	mu   sync.Mutex
	conn *wsconn.Conn
	auth chan string
}

func newRelay(t *testing.T) *relay {
	r := &relay{in: make(chan protocol.Message, 256), auth: make(chan string, 4)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.auth <- req.Header.Get("Authorization")
		ws, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		c := wsconn.New(ws, wsconn.Options{MaxMessageBytes: 1 << 20})
		r.mu.Lock()
		r.conn = c
		r.mu.Unlock()
		c.Send(&protocol.Config{PeerConnectionOptions: []byte(`{}`), ProtocolVersion: protocol.Version})
		c.Send(&protocol.Identify{})
		c.Run(req.Context(), func(m protocol.Message) { r.in <- m }, nil)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *relay) send(m protocol.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conn.Send(m)
}

func loopbackAPI(t *testing.T) *webrtc.API {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		t.Fatal(err)
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		t.Fatal(err)
	}
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
}

func gathered(t *testing.T, pc *webrtc.PeerConnection, desc webrtc.SessionDescription) string {
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(desc); err != nil {
		t.Fatal(err)
	}
	<-done
	return pc.LocalDescription().SDP
}

func negotiated(t *testing.T, pc *webrtc.PeerConnection, label string, id uint16) *webrtc.DataChannel {
	dc, err := negotiatedChannel(pc, label, id)
	if err != nil {
		t.Fatal(err)
	}
	return dc
}

// TestEndToEnd runs a simulcast streamer through the SFU to a player that behaves like the
// frontend, and checks media, layer switching and the data channel relay, both for a multiplexing
// (PS2) streamer and for one that takes a channel per player (PS1).
func TestEndToEnd(t *testing.T) {
	t.Run("multiplexed", func(t *testing.T) { endToEnd(t, true) })
	t.Run("per-player channels", func(t *testing.T) { endToEnd(t, false) })
}

func endToEnd(t *testing.T, multiplex bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := newRelay(t)

	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("s3cret-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SignallingURL = "ws" + strings.TrimPrefix(r.srv.URL, "http")
	cfg.SubscribeStreamerID = "S"
	cfg.TokenFile = tokenFile
	cfg.ICE = ICEConfig{IncludeLoopback: true}
	cfg.GatherTimeout = 3 * time.Second
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	s, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	left := make(chan string, 1)
	s.Hooks.OnPlayerLeave = func(id string) { left <- id }
	go s.Run(ctx)

	if got := <-r.auth; got != "Bearer s3cret-token-value" {
		t.Fatalf("authorization header %q", got)
	}

	api := loopbackAPI(t)

	// The streamer: two VP8 simulcast layers whose payload's third byte names the layer, and the
	// two negotiated channels PS2 uses for multiplexing.
	streamerPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer streamerPC.Close()
	vp8 := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	var layerTracks []*webrtc.TrackLocalStaticRTP
	for _, rid := range []string{"simulcast1", "simulcast0"} { // listed out of order on purpose
		tr, err := webrtc.NewTrackLocalStaticRTP(vp8, "video", "ps", webrtc.WithRTPStreamID(rid))
		if err != nil {
			t.Fatal(err)
		}
		layerTracks = append(layerTracks, tr)
	}
	sender, err := streamerPC.AddTrack(layerTracks[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.AddEncoding(layerTracks[1]); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, _, err := sender.ReadRTCP(); err != nil {
				return
			}
		}
	}()
	relayStatus := make(chan []byte, 4)
	if !multiplex {
		// Only there to put an SCTP m-line in the offer; per-player channels come later.
		negotiated(t, streamerPC, "unused", 900)
		close(relayStatus)
	}
	onPerPlayerChannel := func(id uint16) {
		dc := negotiated(t, streamerPC, "datachannel", id)
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if string(m.Data) != "hello" || m.IsString {
				t.Errorf("streamer got %q (text %v)", m.Data, m.IsString)
				return
			}
			_ = dc.Send([]byte("world"))
		})
	}
	ueSend := negotiated(t, streamerPC, "send-datachannel", 0)
	ueRecv := negotiated(t, streamerPC, "recv-datachannel", 1)
	ueRecv.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.Data[0] == msgChannelRelayStatus {
			relayStatus <- m.Data
			return
		}
		id, payload, err := decodeMultiplexed(m.Data)
		if err != nil || id != "Player1" || string(payload) != "hello" {
			t.Errorf("streamer got %v (%q %q %v)", m.Data, id, payload, err)
			return
		}
		_ = ueSend.Send(encodeMultiplexed("Player1", []byte("world")))
		_ = ueSend.Send(encodeMultiplexed("Player9", []byte("not yours")))
	})
	go func() {
		// Like a browser, the sending side has to tag simulcast packets with mid and rid itself.
		var midID, ridID uint8
		var mid string
		for ctx.Err() == nil && (midID == 0 || ridID == 0 || mid == "") {
			time.Sleep(20 * time.Millisecond)
			for _, ext := range sender.GetParameters().HeaderExtensions {
				switch ext.URI {
				case "urn:ietf:params:rtp-hdrext:sdes:mid":
					midID = uint8(ext.ID)
				case "urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id":
					ridID = uint8(ext.ID)
				}
			}
			mid = streamerPC.GetTransceivers()[0].Mid()
		}
		rids := []string{"simulcast1", "simulcast0"}
		seq := [2]uint16{}
		for i := uint32(0); ctx.Err() == nil; i++ {
			for li, tr := range layerTracks {
				layerIdx := byte(1 - li) // tracks[0] is simulcast1
				key := byte(0x01)
				if i%15 == 0 {
					key = 0x00
				}
				seq[li]++
				h := rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq[li], Timestamp: i * 3000, Marker: true}
				_ = h.SetExtension(midID, []byte(mid))
				_ = h.SetExtension(ridID, []byte(rids[li]))
				_ = tr.WriteRTP(&rtp.Packet{Header: h, Payload: []byte{0x10, key, layerIdx, 0, 0, 0}})
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var playerPC *webrtc.PeerConnection
	defer func() {
		if playerPC != nil {
			playerPC.Close()
		}
	}()
	world := make(chan struct{})
	type rx struct {
		seq   uint16
		layer byte
	}
	media := make(chan rx, 1024)

	sawStatus := false
	for done := false; !done; {
		var m protocol.Message
		select {
		case m = <-r.in:
		case <-world:
			done = true
			continue
		case <-ctx.Done():
			t.Fatal("timed out")
		}
		switch v := m.(type) {
		case *protocol.EndpointID:
			if v.ID != "SFU" {
				t.Fatalf("endpointId %q", v.ID)
			}
			r.send(&protocol.EndpointIDConfirm{CommittedID: "SFU"})
		case *protocol.ListStreamers:
			r.send(&protocol.StreamerList{IDs: []string{"SFU", "Other", "S"}})
		case *protocol.Subscribe:
			if v.StreamerID != "S" {
				t.Fatalf("subscribed to %q", v.StreamerID)
			}
			offer, err := streamerPC.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			// Older signalling servers leave the SFU's own player id on the streamer's offer.
			r.send(&protocol.Offer{SDP: gathered(t, streamerPC, offer), PlayerID: "Player0", Multiplex: &multiplex})
		case *protocol.Answer:
			if err := streamerPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: v.SDP}); err != nil {
				t.Fatal(err)
			}
		case *protocol.StartStreaming:
			r.send(&protocol.PlayerConnected{PlayerID: "Player1", DataChannel: true})
		case *protocol.Offer:
			if v.PlayerID != "Player1" || v.SFU == nil || !*v.SFU {
				t.Fatalf("bad player offer %+v", v)
			}
			playerPC, err = api.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatal(err)
			}
			playerPC.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
				for {
					p, _, err := tr.ReadRTP()
					if err != nil {
						return
					}
					if tr.Kind() == webrtc.RTPCodecTypeVideo && len(p.Payload) > 2 {
						select {
						case media <- rx{p.SequenceNumber, p.Payload[2]}:
						default:
						}
					}
				}
			})
			if err := playerPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: v.SDP}); err != nil {
				t.Fatal(err)
			}
			answer, err := playerPC.CreateAnswer(nil)
			if err != nil {
				t.Fatal(err)
			}
			r.send(&protocol.Answer{PlayerID: "Player1", SDP: gathered(t, playerPC, answer)})
			r.send(&protocol.DataChannelRequest{PlayerID: "Player1"})
		case *protocol.StreamerDataChannels:
			if multiplex || v.PlayerID != "Player1" || v.SendStreamID != v.RecvStreamID {
				t.Fatalf("unexpected %+v", v)
			}
			onPerPlayerChannel(uint16(v.SendStreamID))
		case *protocol.PeerDataChannels:
			if v.SendStreamID == v.RecvStreamID {
				t.Fatal("player channels must be unidirectional")
			}
			send := negotiated(t, playerPC, "send-datachannel", uint16(v.SendStreamID))
			recv := negotiated(t, playerPC, "recv-datachannel", uint16(v.RecvStreamID))
			recv.OnOpen(func() { r.send(&protocol.PeerDataChannelsReady{PlayerID: "Player1"}) })
			send.OnOpen(func() { _ = send.Send([]byte("hello")) })
			recv.OnMessage(func(m webrtc.DataChannelMessage) {
				if string(m.Data) != "world" {
					t.Errorf("player got %q", m.Data)
				}
				close(world)
			})
		}
		if !sawStatus && multiplex {
			select {
			case st := <-relayStatus:
				if !bytes.Equal(st, encodeRelayStatus("Player1", true)) {
					t.Fatalf("relay status %v", st)
				}
				sawStatus = true
			default:
			}
		}
	}

	// Media: the player starts on the largest layer (simulcast0), then follows its preference
	// down to simulcast1, with no gap in sequence numbers across the switch.
	var last *rx
	expect := func(layer byte) {
		deadline := time.After(10 * time.Second)
		for n := 0; ; {
			select {
			case p := <-media:
				if last != nil && p.seq != last.seq+1 {
					t.Fatalf("sequence gap %d -> %d", last.seq, p.seq)
				}
				last = &p
				if p.layer == layer {
					if n++; n >= 10 {
						return
					}
				}
			case <-deadline:
				t.Fatalf("never received layer %d", layer)
			}
		}
	}
	expect(0)
	r.send(&protocol.LayerPreference{PlayerID: "Player1", SpatialLayer: 1})
	expect(1)

	r.send(&protocol.PlayerDisconnected{PlayerID: "Player1"})
	select {
	case id := <-left:
		if id != "Player1" {
			t.Fatalf("left: %q", id)
		}
	case <-ctx.Done():
		t.Fatal("player never left")
	}
	if !multiplex {
		return
	}
	select {
	case st := <-relayStatus:
		if !sawStatus {
			// The open status may have been read only now.
			sawStatus = true
			st = <-relayStatus
		}
		if !bytes.Equal(st, encodeRelayStatus("Player1", false)) {
			t.Fatalf("closing relay status %v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("streamer was not told the player left")
	}
}
