package sfu

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
)

// keyframeInterval is the most often a keyframe is requested of any one layer, however many players
// ask. Keyframes are expensive for everyone watching, so players cannot be allowed to force them at
// will.
const keyframeInterval = 300 * time.Millisecond

// streamer is the SFU's connection to the upstream streamer.
type streamer struct {
	sfu             *SFU
	log             *slog.Logger
	pc              *webrtc.PeerConnection
	multiplex       bool
	hasData         bool
	scalabilityMode string
	ctx             context.Context
	cancel          context.CancelFunc

	connected  chan struct{} // closed when the peer connection connects
	videoReady chan struct{} // closed when the first video layer arrives
	dataReady  chan struct{} // closed when the multiplexed channels are open (or immediately if none)

	mu      sync.Mutex
	audio   *layer
	video   []*layer // largest first
	toUE    *webrtc.DataChannel
	lastPLI map[uint32]time.Time
	usedIDs map[uint16]bool
	once    map[string]bool
}

func newStreamer(s *SFU, o *protocol.Offer) (*streamer, error) {
	pc, err := s.upstream.NewPeerConnection(s.rtcConfig())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := &streamer{
		sfu: s, log: s.log.With("peer", "streamer"), pc: pc, ctx: ctx, cancel: cancel,
		multiplex:       o.Multiplex != nil && *o.Multiplex,
		scalabilityMode: o.ScalabilityMode,
		hasData:         strings.Contains(o.SDP, "m=application"),
		connected:       make(chan struct{}),
		videoReady:      make(chan struct{}),
		dataReady:       make(chan struct{}),
		lastPLI:         map[uint32]time.Time{},
		usedIDs:         map[uint16]bool{},
		once:            map[string]bool{},
	}
	if st.scalabilityMode == "" {
		st.scalabilityMode = "L1T1"
	}
	pc.OnTrack(func(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		defer s.recover("streamer track")
		st.onTrack(t, r)
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		st.log.Info("connection state", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			st.closeOnce("connected", st.connected)
		case webrtc.PeerConnectionStateFailed:
			go s.streamerFailed(st)
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP}); err != nil {
		pc.Close()
		return nil, err
	}
	if st.hasData && st.multiplex {
		if err := st.openMultiplexChannels(); err != nil {
			pc.Close()
			return nil, err
		}
	} else {
		close(st.dataReady)
	}
	answer, err := pc.CreateAnswer(nil)
	if err == nil {
		err = pc.SetLocalDescription(answer)
	}
	if err != nil {
		pc.Close()
		return nil, err
	}
	go st.sampleRates()
	return st, nil
}

func (st *streamer) closeOnce(name string, ch chan struct{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.once[name] {
		st.once[name] = true
		close(ch)
	}
}

// answer sends our answer once candidates are gathered, then announces that we are streaming once
// media and data are flowing.
func (st *streamer) answer() {
	defer st.sfu.recover("streamer answer")
	sdp := waitForCandidates(st.ctx, st.pc, st.sfu.cfg.GatherTimeout)
	if sdp == "" {
		return
	}
	st.sfu.send(&protocol.Answer{SDP: sdp})

	select {
	case <-st.connected:
	case <-st.ctx.Done():
		return
	}
	// Video normally arrives at once. If it does not, start anyway: an audio-only or idle
	// streamer is still a streamer, and players get video when it starts.
	timeout := time.After(5 * time.Second)
	for _, ch := range []chan struct{}{st.videoReady, st.dataReady} {
		select {
		case <-ch:
		case <-timeout:
		case <-st.ctx.Done():
			return
		}
	}
	st.log.Info("streaming", "multiplex", st.multiplex, "layers", len(st.layers()))
	st.sfu.send(&protocol.StartStreaming{})
	if h := st.sfu.Hooks.OnStreamerConnected; h != nil {
		h()
	}
}

// waitForCandidates returns the local SDP once ICE gathering completes or the timeout passes.
func waitForCandidates(ctx context.Context, pc *webrtc.PeerConnection, timeout time.Duration) string {
	done := webrtc.GatheringCompletePromise(pc)
	select {
	case <-done:
	case <-time.After(timeout):
	case <-ctx.Done():
		return ""
	}
	if d := pc.LocalDescription(); d != nil {
		return d.SDP
	}
	return ""
}

func (st *streamer) addCandidate(c protocol.IceCandidateData) {
	if err := addCandidate(st.pc, c); err != nil {
		st.log.Warn("bad ice candidate", "err", err)
	}
}

func addCandidate(pc *webrtc.PeerConnection, c protocol.IceCandidateData) error {
	if c.Candidate == "" {
		return nil // end of candidates
	}
	idx := uint16(c.SdpMLineIndex)
	return pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate: c.Candidate, SDPMid: &c.SdpMid, SDPMLineIndex: &idx, UsernameFragment: c.UsernameFragment,
	})
}

func (st *streamer) onTrack(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
	l := &layer{rid: t.RID(), kind: t.Kind(), ssrc: uint32(t.SSRC()), codec: t.Codec()}
	st.mu.Lock()
	if l.kind == webrtc.RTPCodecTypeAudio {
		st.audio = l
	} else {
		st.video = append(st.video, l)
		ridOrder(st.video)
	}
	st.mu.Unlock()
	st.log.Info("track", "kind", l.kind.String(), "rid", l.rid, "codec", l.codec.MimeType, "ssrc", l.ssrc)
	if l.kind == webrtc.RTPCodecTypeVideo {
		st.closeOnce("video", st.videoReady)
	}

	go func() { // receiver RTCP must be read for the interceptors to see it
		for {
			var err error
			if l.rid != "" {
				_, _, err = r.ReadSimulcastRTCP(l.rid)
			} else {
				_, _, err = r.ReadRTCP()
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		p, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		l.bytes.Add(uint64(p.MarshalSize()))
		for _, pl := range *st.sfu.view.Load() {
			pl.forward(l, p)
		}
	}
}

func (st *streamer) sampleRates() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-st.ctx.Done():
			return
		case <-t.C:
			for _, l := range st.layers() {
				l.sample(time.Second)
			}
		}
	}
}

// layers returns the video layers, largest first.
func (st *streamer) layers() []*layer {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]*layer(nil), st.video...)
}

func (st *streamer) audioLayer() *layer {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.audio
}

// requestKeyframe asks the streamer for a keyframe on a layer, at most once per keyframeInterval.
func (st *streamer) requestKeyframe(l *layer) {
	if l == nil {
		return
	}
	st.mu.Lock()
	if time.Since(st.lastPLI[l.ssrc]) < keyframeInterval {
		st.mu.Unlock()
		return
	}
	st.lastPLI[l.ssrc] = time.Now()
	st.mu.Unlock()
	_ = st.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: l.ssrc}})
}

// openMultiplexChannels creates the two channels PS2 uses for all SFU players.
func (st *streamer) openMultiplexChannels() error {
	fromUE, err := negotiatedChannel(st.pc, "send-datachannel", muxStreamerSendID)
	if err != nil {
		return err
	}
	toUE, err := negotiatedChannel(st.pc, "recv-datachannel", muxStreamerRecvID)
	if err != nil {
		return err
	}
	st.toUE = toUE
	toUE.OnOpen(func() { st.closeOnce("data", st.dataReady) })
	fromUE.OnMessage(func(m webrtc.DataChannelMessage) {
		defer st.sfu.recover("streamer data")
		id, payload, err := decodeMultiplexed(m.Data)
		if err != nil {
			st.log.Debug("dropping streamer data message without a player header", "type", firstByte(m.Data))
			return
		}
		if p := st.sfu.player(id); p != nil && p.st == st {
			p.toPlayerMsg(dcMessage{data: payload})
		}
	})
	return nil
}

// sendMultiplexed sends a framed message to the streamer in multiplex mode.
func (st *streamer) sendMultiplexed(data []byte) bool {
	st.mu.Lock()
	dc := st.toUE
	st.mu.Unlock()
	return sendDC(dc, dcMessage{data: data})
}

// openPlayerChannel creates a per-player channel to a non-multiplexing (PS1) streamer.
func (st *streamer) openPlayerChannel(p *player) (*webrtc.DataChannel, uint16, error) {
	st.mu.Lock()
	id := uint16(0)
	for i := uint16(firstStreamerChannelID); i <= lastStreamerChannelID; i++ {
		if !st.usedIDs[i] {
			id = i
			break
		}
	}
	if id == 0 {
		st.mu.Unlock()
		return nil, 0, errors.New("sfu: no free data channel ids on the streamer connection")
	}
	st.usedIDs[id] = true
	st.mu.Unlock()
	dc, err := negotiatedChannel(st.pc, "datachannel", id)
	if err != nil {
		st.releaseChannel(id)
		return nil, 0, err
	}
	return dc, id, nil
}

func (st *streamer) releaseChannel(id uint16) {
	st.mu.Lock()
	delete(st.usedIDs, id)
	st.mu.Unlock()
}

func (st *streamer) close() {
	st.cancel()
	_ = st.pc.Close()
}

func negotiatedChannel(pc *webrtc.PeerConnection, label string, id uint16) (*webrtc.DataChannel, error) {
	yes := true
	return pc.CreateDataChannel(label, &webrtc.DataChannelInit{Negotiated: &yes, ID: &id, Ordered: &yes})
}

func firstByte(b []byte) int {
	if len(b) == 0 {
		return -1
	}
	return int(b[0])
}
