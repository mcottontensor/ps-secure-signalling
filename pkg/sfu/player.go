package sfu

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
)

// selectInterval is how often a player's layer is reconsidered.
const selectInterval = 500 * time.Millisecond

// player is one downstream peer. Everything it holds is released by close.
type player struct {
	id     string
	sfu    *SFU
	st     *streamer
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	built  chan struct{} // closed once pc is set up (or setup failed)
	active bool          // receiving media; guarded by sfu.mu

	pc    *webrtc.PeerConnection
	bwe   cc.BandwidthEstimator
	audio *forwarder
	video *forwarder

	mu           sync.Mutex
	selector     *layerSelector
	toPlayer     *webrtc.DataChannel
	streamerDC   *webrtc.DataChannel // PS1 only
	streamerDCID uint16
	ready        bool // the player's receiving channel is open
	relayOpen    bool // PS2: the streamer has been told about this player
	toPlayerQ    pendingQueue
	toStreamerQ  pendingQueue
	closed       bool
}

func newPlayer(s *SFU, st *streamer, id string) *player {
	ctx, cancel := context.WithCancel(st.ctx)
	return &player{
		id: id, sfu: s, st: st, log: s.log.With("player", id), ctx: ctx, cancel: cancel,
		built: make(chan struct{}), selector: newLayerSelector(0),
	}
}

// start builds the peer connection and sends the player our offer.
func (p *player) start() {
	defer p.sfu.recover("player " + p.id)
	err := p.build()
	close(p.built)
	if err != nil {
		p.log.Error("could not set up player", "err", err)
		p.sfu.removePlayer(p.id, "setup failed")
		return
	}
	p.sfu.activate(p)
	sdp := waitForCandidates(p.ctx, p.pc, p.sfu.cfg.GatherTimeout)
	if sdp == "" {
		return
	}
	yes := true
	p.sfu.send(&protocol.Offer{PlayerID: protocol.PlayerID(p.id), SDP: sdp, SFU: &yes, ScalabilityMode: p.st.scalabilityMode})
	p.selectLoop()
}

func (p *player) build() error {
	// A player can subscribe in the moment between the streamer connecting and its video
	// arriving. Give it a few seconds rather than offering it a stream with no video.
	select {
	case <-p.st.videoReady:
	case <-time.After(5 * time.Second):
		return errors.New("streamer has no video")
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
	layers, audio := p.st.layers(), p.st.audioLayer()
	codecs := []webrtc.RTPCodecParameters{layers[0].codec}
	if audio != nil {
		codecs = append(codecs, audio.codec)
	}
	pc, est, err := newPlayerPC(p.sfu.settings, p.sfu.rtcConfig(), codecs, p.sfu.cfg.BWE)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.pc, p.bwe = pc, est
	closed := p.closed
	p.mu.Unlock()
	if closed {
		_ = pc.Close()
		return errors.New("closed during setup")
	}

	var senders []*webrtc.RTPSender
	video, vs, err := p.addTrack(layers[0].codec, "video")
	if err != nil {
		return err
	}
	senders = append(senders, vs)
	video.setTarget(layers[0])
	p.video = video
	if audio != nil {
		a, as, err := p.addTrack(audio.codec, "audio")
		if err != nil {
			return err
		}
		senders = append(senders, as)
		a.setTarget(audio)
		p.audio = a
	}
	for _, s := range senders {
		go p.readRTCP(s)
	}

	// The SCTP m-line has to be in the offer; the channels themselves are pre-negotiated with the
	// ids announced later in peerDataChannels.
	fromPlayer, err := negotiatedChannel(pc, "send-datachannel", playerSendID)
	if err != nil {
		return err
	}
	toPlayer, err := negotiatedChannel(pc, "recv-datachannel", playerRecvID)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.toPlayer = toPlayer
	p.mu.Unlock()
	fromPlayer.OnMessage(func(m webrtc.DataChannelMessage) {
		defer p.sfu.recover("player data " + p.id)
		p.fromPlayerMsg(dcMessage{data: m.Data, isText: m.IsString})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		p.log.Info("connection state", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			p.st.requestKeyframe(p.video.pending())
		case webrtc.PeerConnectionStateFailed:
			go p.sfu.removePlayer(p.id, "connection failed")
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	return pc.SetLocalDescription(offer)
}

func (p *player) addTrack(codec webrtc.RTPCodecParameters, kind string) (*forwarder, *webrtc.RTPSender, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(codec.RTPCodecCapability, kind, "pixelstreaming")
	if err != nil {
		return nil, nil, err
	}
	tr, err := p.pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		return nil, nil, err
	}
	// One codec only: the SFU forwards what the streamer sends and cannot transcode.
	if err := tr.SetCodecPreferences([]webrtc.RTPCodecParameters{codec}); err != nil {
		return nil, nil, err
	}
	return newForwarder(track, codec), tr.Sender(), nil
}

// readRTCP services a sender's RTCP. Reading drives the NACK and congestion control interceptors;
// keyframe requests are passed on to the streamer for the layer this player is watching.
func (p *player) readRTCP(s *webrtc.RTPSender) {
	defer p.sfu.recover("player rtcp " + p.id)
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if p.video != nil {
					l := p.video.pending()
					if l == nil {
						l = p.video.currentLayer()
					}
					p.st.requestKeyframe(l)
				}
			}
		}
	}
}

// forward is called for every packet the streamer sends, on the streamer's goroutines.
func (p *player) forward(l *layer, pkt *rtp.Packet) {
	var f *forwarder
	if l.kind == webrtc.RTPCodecTypeAudio {
		f = p.audio
	} else {
		f = p.video
	}
	if f != nil {
		_ = f.write(l, pkt)
	}
}

// selectLoop keeps the player on the best layer its preference and bandwidth allow, and keeps
// asking for a keyframe while a switch is waiting for one.
func (p *player) selectLoop() {
	t := time.NewTicker(selectInterval)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
		}
		layers := p.st.layers()
		rates := make([]int64, len(layers))
		for i, l := range layers {
			rates[i] = l.bitrate.Load()
		}
		estimate := 0
		if p.bwe != nil {
			estimate = p.bwe.GetTargetBitrate()
		}
		p.mu.Lock()
		before := p.selector.current
		idx := p.selector.next(time.Now(), rates, estimate)
		p.mu.Unlock()
		if idx != before {
			p.log.Debug("layer change", "from", before, "to", idx, "estimate", estimate)
		}
		p.video.setTarget(layers[idx])
		p.st.requestKeyframe(p.video.pending())
	}
}

// setCeiling applies the player's layerPreference: the largest layer it wants, 0 being the largest.
func (p *player) setCeiling(spatial int) {
	p.mu.Lock()
	p.selector.ceiling = max(spatial, 0) // clamped to the layers that exist when next used
	p.mu.Unlock()
}

func (p *player) waitBuilt() bool {
	select {
	case <-p.built:
		return p.pc != nil
	case <-p.ctx.Done():
		return false
	}
}

func (p *player) setAnswer(sdp string) {
	if !p.waitBuilt() {
		return
	}
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		p.log.Warn("bad answer", "err", err)
		p.sfu.removePlayer(p.id, "bad answer")
	}
}

func (p *player) addCandidate(c protocol.IceCandidateData) {
	if !p.waitBuilt() {
		return
	}
	if err := addCandidate(p.pc, c); err != nil {
		p.log.Warn("bad ice candidate", "err", err)
	}
}

// onDataChannelRequest tells the player which channels to open, and (PS2) tells the streamer the
// player is here so it can talk to it as soon as the channels are up.
func (p *player) onDataChannelRequest() {
	if !p.waitBuilt() || !p.st.hasData {
		return
	}
	if p.st.multiplex {
		p.mu.Lock()
		first := !p.relayOpen
		p.relayOpen = true
		p.mu.Unlock()
		if first {
			p.st.sendMultiplexed(encodeRelayStatus(p.id, true))
		}
	}
	p.sfu.send(&protocol.PeerDataChannels{
		PlayerID: protocol.PlayerID(p.id), SendStreamID: playerSendID, RecvStreamID: playerRecvID,
	})
}

// onDataChannelsReady runs when the player's receiving channel is open: queued streamer messages
// can go, and a PS1 streamer is given a channel for this player.
func (p *player) onDataChannelsReady() {
	if !p.waitBuilt() {
		return
	}
	p.mu.Lock()
	p.ready = true
	queued, dc := p.toPlayerQ.drain(), p.toPlayer
	needChannel := !p.st.multiplex && p.st.hasData && p.streamerDC == nil && !p.closed
	p.mu.Unlock()
	for _, m := range queued {
		sendDC(dc, m)
	}
	if !needChannel {
		return
	}
	sdc, id, err := p.st.openPlayerChannel(p)
	if err != nil {
		p.log.Error("no data channel to the streamer", "err", err)
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = sdc.Close()
		p.st.releaseChannel(id)
		return
	}
	p.streamerDC, p.streamerDCID = sdc, id
	p.mu.Unlock()

	// The streamer creates its end when it gets streamerDataChannels. Messages sent before then
	// could be lost, so queued ones wait until it first speaks, or two seconds.
	var flushOnce sync.Once
	flush := func() {
		flushOnce.Do(func() {
			p.mu.Lock()
			queued := p.toStreamerQ.drain()
			p.mu.Unlock()
			for _, m := range queued {
				sendDC(sdc, m)
			}
		})
	}
	sdc.OnMessage(func(m webrtc.DataChannelMessage) {
		defer p.sfu.recover("streamer data " + p.id)
		flush()
		p.toPlayerMsg(dcMessage{data: m.Data, isText: m.IsString})
	})
	time.AfterFunc(2*time.Second, flush)
	p.sfu.send(&protocol.StreamerDataChannels{PlayerID: protocol.PlayerID(p.id), SendStreamID: protocol.Int(id), RecvStreamID: protocol.Int(id)})
}

// fromPlayerMsg relays a message from the player to the streamer. The player id in a multiplex
// header comes from this session, never from the player, so players cannot speak for each other.
func (p *player) fromPlayerMsg(m dcMessage) {
	if len(m.data) == 0 || len(m.data) > p.sfu.cfg.MaxDataChannelMessage {
		return
	}
	if p.st.multiplex {
		p.st.sendMultiplexed(encodeMultiplexed(p.id, m.data))
		return
	}
	p.mu.Lock()
	dc := p.streamerDC
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen || len(p.toStreamerQ.msgs) > 0 {
		p.toStreamerQ.push(m)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	sendDC(dc, m)
}

// toPlayerMsg relays a message from the streamer to the player, holding it until the player's
// receiving channel exists.
func (p *player) toPlayerMsg(m dcMessage) {
	if len(m.data) > p.sfu.cfg.MaxDataChannelMessage {
		return
	}
	p.mu.Lock()
	if !p.ready {
		p.toPlayerQ.push(m)
		p.mu.Unlock()
		return
	}
	dc := p.toPlayer
	p.mu.Unlock()
	sendDC(dc, m)
}

func (p *player) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	pc, sdc, sid, relay := p.pc, p.streamerDC, p.streamerDCID, p.relayOpen
	p.mu.Unlock()

	p.cancel()
	if pc != nil {
		_ = pc.Close()
	}
	if sdc != nil {
		_ = sdc.Close()
		p.st.releaseChannel(sid)
	}
	if relay {
		p.st.sendMultiplexed(encodeRelayStatus(p.id, false))
	}
	if h := p.sfu.Hooks.OnPlayerLeave; h != nil {
		h(p.id)
	}
}
