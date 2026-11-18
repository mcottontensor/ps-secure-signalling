// Command teststreamer is a synthetic Pixel Streaming streamer, for testing signalling servers,
// SFUs and deployments without Unreal Engine. It speaks the streamer side of the signalling
// protocol the way Pixel Streaming 2 does:
//
//   - VP8 video from IVF files, as simulcast to SFUs (largest layer first, rids simulcast0..N-1)
//     and as the largest layer alone to direct players
//   - optional Opus audio from an Ogg file
//   - an in-band data channel to direct players, and the multiplexed pair of negotiated channels
//     (UE sends on stream 0, receives on stream 1) to SFUs
//   - replies to the frontend's initial settings request and latency test on the data channel
//
// Make media with deploy/make-test-media.sh.
package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/coder/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/config"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/wsconn"
)

// Data channel message ids (Pixel Streaming 2 default protocol).
const (
	toStreamerLatencyTest     = 6
	toStreamerInitialSettings = 7
	fromStreamerLatencyTest   = 6
	fromStreamerInitial       = 7
	relayStatus               = 198
	multiplexed               = 199
)

type streamer struct {
	log    *slog.Logger
	api    *webrtc.API
	iceCfg webrtc.Configuration
	conn   *wsconn.Conn
	direct *webrtc.TrackLocalStaticSample   // largest layer, for direct players
	layers []*webrtc.TrackLocalStaticSample // simulcast, largest first
	audio  *webrtc.TrackLocalStaticSample
	tags   *simulcastTagger
	mu     sync.Mutex
	peers  map[protocol.PlayerID]*webrtc.PeerConnection
}

func main() {
	url := flag.String("url", "ws://127.0.0.1:8888", "signalling server streamer URL")
	id := flag.String("id", "DefaultStreamer", "streamer id to request")
	tokenFile := flag.String("token-file", "", "file holding a bearer token for the signalling server")
	mediaDir := flag.String("media", "testmedia", "directory with video_0.ivf (largest), video_1.ivf, ... and optionally audio.ogg")
	var tlsCfg config.ClientTLS
	flag.StringVar(&tlsCfg.CAFile, "ca-file", "", "CA bundle to verify a wss:// signalling server")
	flag.StringVar(&tlsCfg.CertFile, "cert-file", "", "client certificate, for mutual TLS")
	flag.StringVar(&tlsCfg.KeyFile, "key-file", "", "client certificate key")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *url, *id, *tokenFile, *mediaDir, tlsCfg); err != nil {
		log.Error("teststreamer failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, url, id, tokenFile, mediaDir string, clientTLS config.ClientTLS) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return err
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		return err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return err
	}
	tags := &simulcastTagger{}
	reg.Add(tags)
	s := &streamer{log: log, api: webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg)), tags: tags, peers: map[protocol.PlayerID]*webrtc.PeerConnection{}}

	vp8 := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	files, _ := filepath.Glob(filepath.Join(mediaDir, "video_*.ivf"))
	if len(files) == 0 {
		return fmt.Errorf("no video_*.ivf in %s (run deploy/make-test-media.sh)", mediaDir)
	}
	var err error
	if s.direct, err = webrtc.NewTrackLocalStaticSample(vp8, "video", "pixelstreaming_av_stream_id"); err != nil {
		return err
	}
	for i := range files {
		t, err := webrtc.NewTrackLocalStaticSample(vp8, "video", "pixelstreaming_av_stream_id", webrtc.WithRTPStreamID(fmt.Sprintf("simulcast%d", i)))
		if err != nil {
			return err
		}
		s.layers = append(s.layers, t)
	}
	for i := range files {
		targets := []*webrtc.TrackLocalStaticSample{s.layers[i]}
		if i == 0 {
			targets = append(targets, s.direct)
		}
		go playIVF(ctx, log, filepath.Join(mediaDir, fmt.Sprintf("video_%d.ivf", i)), targets)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "audio.ogg")); err == nil {
		if s.audio, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "pixelstreaming_av_stream_id"); err != nil {
			return err
		}
		go playOgg(ctx, log, filepath.Join(mediaDir, "audio.ogg"), s.audio)
	}

	header := http.Header{}
	if tokenFile != "" {
		tok, err := config.Secret("", tokenFile)
		if err != nil {
			return err
		}
		header.Set("Authorization", "Bearer "+string(tok))
	}
	tlsCfg, err := clientTLS.Client()
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		if err := s.session(ctx, url, id, header, tlsCfg); err != nil {
			log.Warn("signalling session ended", "err", err)
		}
		s.closeAll()
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

func (s *streamer) session(ctx context.Context, url, id string, header http.Header, tlsCfg *tls.Config) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	})
	if err != nil {
		return err
	}
	s.conn = wsconn.New(ws, wsconn.Options{MessagesPerSecond: 1000, Burst: 1000})
	s.log.Info("connected to signalling", "url", url)
	s.conn.Run(ctx, func(m protocol.Message) { s.handle(m, id) }, func(err error) { s.log.Warn("bad message", "err", err) })
	return errors.New("disconnected")
}

func (s *streamer) handle(m protocol.Message, id string) {
	switch v := m.(type) {
	case *protocol.Config:
		var cfg webrtc.Configuration
		if json.Unmarshal(v.PeerConnectionOptions, &cfg) == nil {
			s.iceCfg = webrtc.Configuration{ICEServers: cfg.ICEServers}
		}
	case *protocol.Identify:
		s.conn.Send(&protocol.EndpointID{ID: id, ProtocolVersion: protocol.Version})
	case *protocol.EndpointIDConfirm:
		s.log.Info("registered", "id", v.CommittedID)
	case *protocol.PlayerConnected:
		if err := s.addPeer(v.PlayerID, v.SFU); err != nil {
			s.log.Error("could not offer to player", "player", v.PlayerID, "err", err)
		}
	case *protocol.Answer:
		if pc := s.peer(v.PlayerID); pc != nil {
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: v.SDP}); err != nil {
				s.log.Error("bad answer", "player", v.PlayerID, "err", err)
			}
		}
	case *protocol.IceCandidate:
		if pc := s.peer(v.PlayerID); pc != nil {
			mid, idx := v.Candidate.SdpMid, uint16(v.Candidate.SdpMLineIndex)
			_ = pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: v.Candidate.Candidate, SDPMid: &mid, SDPMLineIndex: &idx})
		}
	case *protocol.PlayerDisconnected:
		s.mu.Lock()
		if pc := s.peers[v.PlayerID]; pc != nil {
			_ = pc.Close()
			delete(s.peers, v.PlayerID)
		}
		s.mu.Unlock()
		s.log.Info("player left", "player", v.PlayerID)
	}
}

func (s *streamer) peer(id protocol.PlayerID) *webrtc.PeerConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peers[id]
}

func (s *streamer) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, pc := range s.peers {
		_ = pc.Close()
		delete(s.peers, id)
	}
}

func (s *streamer) addPeer(playerID protocol.PlayerID, sfu bool) error {
	pc, err := s.api.NewPeerConnection(s.iceCfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if old := s.peers[playerID]; old != nil {
		_ = old.Close()
	}
	s.peers[playerID] = pc
	s.mu.Unlock()

	if sfu {
		tr, err := pc.AddTransceiverFromTrack(s.layers[0], webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			return err
		}
		for _, l := range s.layers[1:] {
			if err := tr.Sender().AddEncoding(l); err != nil {
				return err
			}
		}
		for _, enc := range tr.Sender().GetParameters().Encodings {
			s.tags.streams.Store(uint32(enc.SSRC), simulcastTag{rid: enc.RID, transceiver: tr})
		}
		drainRTCP(tr.Sender())
		if err := s.multiplexChannels(pc); err != nil {
			return err
		}
	} else {
		snd, err := pc.AddTrack(s.direct)
		if err != nil {
			return err
		}
		drainRTCP(snd)
		dc, err := pc.CreateDataChannel("datachannel", nil)
		if err != nil {
			return err
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if reply := s.reply(msg.Data); reply != nil {
				_ = dc.Send(reply)
			}
		})
	}
	if s.audio != nil {
		snd, err := pc.AddTrack(s.audio)
		if err != nil {
			return err
		}
		drainRTCP(snd)
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		cand := protocol.IceCandidateData{Candidate: init.Candidate}
		if init.SDPMid != nil {
			cand.SdpMid = *init.SDPMid
		}
		if init.SDPMLineIndex != nil {
			cand.SdpMLineIndex = protocol.Int(*init.SDPMLineIndex)
		}
		s.conn.Send(&protocol.IceCandidate{PlayerID: playerID, Candidate: cand})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.log.Info("peer connection", "player", playerID, "state", st.String())
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	msg := &protocol.Offer{PlayerID: playerID, SDP: offer.SDP}
	if sfu {
		yes := true
		msg.Multiplex = &yes
	}
	s.conn.Send(msg)
	s.log.Info("offered", "player", playerID, "sfu", sfu)
	return nil
}

// multiplexChannels creates the negotiated channel pair Pixel Streaming 2 uses with an SFU: it sends
// on stream 0 and receives on stream 1, and every message is prefixed with the player it concerns.
func (s *streamer) multiplexChannels(pc *webrtc.PeerConnection) error {
	negotiated := true
	id0, id1 := uint16(0), uint16(1)
	send, err := pc.CreateDataChannel("send-datachannel", &webrtc.DataChannelInit{Negotiated: &negotiated, ID: &id0})
	if err != nil {
		return err
	}
	recv, err := pc.CreateDataChannel("recv-datachannel", &webrtc.DataChannelInit{Negotiated: &negotiated, ID: &id1})
	if err != nil {
		return err
	}
	recv.OnMessage(func(msg webrtc.DataChannelMessage) {
		player, payload, ok := parsePlayerFrame(msg.Data)
		if !ok {
			return
		}
		switch msg.Data[0] {
		case relayStatus:
			s.log.Info("relay status", "player", player, "open", len(payload) > 0 && payload[0] == 1)
		case multiplexed:
			if reply := s.reply(payload); reply != nil {
				_ = send.Send(append(playerFrame(multiplexed, player), reply...))
			}
		}
	})
	return nil
}

// reply answers the data channel requests the frontend makes on connect and in a latency test.
func (s *streamer) reply(msg []byte) []byte {
	if len(msg) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	switch msg[0] {
	case toStreamerLatencyTest:
		return utf16Message(fromStreamerLatencyTest, fmt.Sprintf(
			`{"ReceiptTimeMs":%d,"TransmissionTimeMs":%d,"PreCaptureTimeMs":%d,"PostCaptureTimeMs":%d,"PreEncodeTimeMs":%d,"PostEncodeTimeMs":%d}`,
			now, now, now, now, now, now))
	case toStreamerInitialSettings:
		return utf16Message(fromStreamerInitial,
			`{"PixelStreaming":{"AllowPixelStreamingCommands":false,"DisableLatencyTest":false},"Encoder":{"TargetBitrate":-1,"MinQuality":0,"MaxQuality":100},"WebRTC":{"FPS":30,"MinBitrate":100000,"MaxBitrate":20000000},"ConfigOptions":{}}`)
	}
	return nil
}

func utf16Message(id byte, s string) []byte {
	out := []byte{id}
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// playerFrame builds [id][byte length u16le][player id UTF-16LE].
func playerFrame(id byte, player string) []byte {
	units := utf16.Encode([]rune(player))
	out := binary.LittleEndian.AppendUint16([]byte{id}, uint16(len(units)*2))
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

func parsePlayerFrame(b []byte) (player string, payload []byte, ok bool) {
	if len(b) < 3 {
		return "", nil, false
	}
	n := int(binary.LittleEndian.Uint16(b[1:3]))
	if n%2 != 0 || len(b) < 3+n {
		return "", nil, false
	}
	units := make([]uint16, n/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[3+2*i:])
	}
	return string(utf16.Decode(units)), b[3+n:], true
}

// simulcastTagger adds the mid and rid header extensions to outgoing simulcast packets, which a
// receiver needs to tell the layers apart. Browsers and libwebrtc (so UE) do this themselves; Pion
// leaves it to the application.
type simulcastTagger struct {
	interceptor.NoOp
	streams sync.Map // SSRC -> simulcastTag
}

type simulcastTag struct {
	rid         string
	transceiver *webrtc.RTPTransceiver
}

func (t *simulcastTagger) NewInterceptor(string) (interceptor.Interceptor, error) { return t, nil }

func (t *simulcastTagger) BindLocalStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	v, ok := t.streams.Load(info.SSRC)
	if !ok {
		return w
	}
	tag := v.(simulcastTag)
	var midID, ridID uint8
	for _, e := range info.RTPHeaderExtensions {
		switch e.URI {
		case sdp.SDESMidURI:
			midID = uint8(e.ID)
		case sdp.SDESRTPStreamIDURI:
			ridID = uint8(e.ID)
		}
	}
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		if mid := tag.transceiver.Mid(); mid != "" && midID != 0 {
			_ = h.SetExtension(midID, []byte(mid))
		}
		if ridID != 0 {
			_ = h.SetExtension(ridID, []byte(tag.rid))
		}
		return w.Write(h, payload, a)
	})
}

func drainRTCP(snd *webrtc.RTPSender) {
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := snd.Read(buf); err != nil {
				return
			}
		}
	}()
}

// playIVF loops an IVF file into tracks at its own frame rate.
func playIVF(ctx context.Context, log *slog.Logger, file string, tracks []*webrtc.TrackLocalStaticSample) {
	for ctx.Err() == nil {
		f, err := os.Open(file)
		if err != nil {
			log.Error("video", "err", err)
			return
		}
		r, hdr, err := ivfreader.NewWith(f)
		if err != nil {
			log.Error("video", "file", file, "err", err)
			f.Close()
			return
		}
		frame := time.Second * time.Duration(hdr.TimebaseNumerator) / time.Duration(hdr.TimebaseDenominator)
		tick := time.NewTicker(frame)
		for ctx.Err() == nil {
			data, _, err := r.ParseNextFrame()
			if err != nil {
				break
			}
			<-tick.C
			for _, t := range tracks {
				_ = t.WriteSample(media.Sample{Data: data, Duration: frame})
			}
		}
		tick.Stop()
		f.Close()
	}
}

// playOgg loops an Ogg Opus file into a track.
func playOgg(ctx context.Context, log *slog.Logger, file string, track *webrtc.TrackLocalStaticSample) {
	for ctx.Err() == nil {
		f, err := os.Open(file)
		if err != nil {
			log.Error("audio", "err", err)
			return
		}
		r, _, err := oggreader.NewWith(f)
		if err != nil {
			log.Error("audio", "err", err)
			f.Close()
			return
		}
		var last uint64
		tick := time.NewTicker(20 * time.Millisecond)
		for ctx.Err() == nil {
			page, hdr, err := r.ParseNextPage()
			if errors.Is(err, io.EOF) || err != nil {
				break
			}
			samples := hdr.GranulePosition - last
			last = hdr.GranulePosition
			<-tick.C
			_ = track.WriteSample(media.Sample{Data: page, Duration: time.Duration(float64(samples)/48000*1e9) * time.Nanosecond})
		}
		tick.Stop()
		f.Close()
	}
}
