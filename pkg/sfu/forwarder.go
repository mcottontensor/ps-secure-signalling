package sfu

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// layer is one incoming RTP stream from the streamer: the audio track, or one simulcast encoding
// (or the only encoding) of the video track.
type layer struct {
	rid   string
	kind  webrtc.RTPCodecType
	ssrc  uint32
	codec webrtc.RTPCodecParameters

	bytes   atomic.Uint64 // received since the last rate sample
	bitrate atomic.Int64  // bits per second, smoothed
}

// sample folds the bytes received over interval into the smoothed bitrate.
func (l *layer) sample(interval time.Duration) {
	bps := int64(float64(l.bytes.Swap(0)*8) / interval.Seconds())
	old := l.bitrate.Load()
	if old == 0 {
		l.bitrate.Store(bps)
		return
	}
	l.bitrate.Store((old*3 + bps) / 4)
}

// ridOrder sorts simulcast layers largest first. Both plugins number layers from the largest:
// PS2 uses simulcast0..N-1 and PS1 simulcast1..N, so ascending numeric suffix is largest first,
// whatever order the encodings were listed in the SDP.
func ridOrder(layers []*layer) {
	num := func(rid string) int {
		i := len(rid)
		for i > 0 && rid[i-1] >= '0' && rid[i-1] <= '9' {
			i--
		}
		n, err := strconv.Atoi(rid[i:])
		if err != nil {
			return 1 << 30
		}
		return n
	}
	sort.SliceStable(layers, func(a, b int) bool {
		na, nb := num(layers[a].rid), num(layers[b].rid)
		if na != nb {
			return na < nb
		}
		return strings.Compare(layers[a].rid, layers[b].rid) < 0
	})
}

// rtpWriter is the part of a local track the forwarder needs.
type rtpWriter interface{ WriteRTP(*rtp.Packet) error }

// forwarder writes one player's copy of a stream. It follows a single source layer at a time and
// rewrites sequence numbers and timestamps so that switching layers looks, to the player, like one
// continuous stream. A switch waits for a keyframe on the new layer; until then the old layer
// keeps flowing.
type forwarder struct {
	out        rtpWriter
	isKeyframe keyframeFunc
	clockRate  uint32
	now        func() time.Time

	mu      sync.Mutex
	current *layer // layer being forwarded, nil until the first keyframe
	target  *layer // layer wanted
	started bool
	lastSeq uint16
	lastTS  uint32
	lastAt  time.Time
	seqOff  uint16
	tsOff   uint32
}

func newForwarder(out rtpWriter, codec webrtc.RTPCodecParameters) *forwarder {
	clock := codec.ClockRate
	if clock == 0 {
		clock = 90000
	}
	return &forwarder{out: out, isKeyframe: keyframeDetector(codec.MimeType), clockRate: clock, now: time.Now}
}

// setTarget selects the layer to forward from the next keyframe on.
func (f *forwarder) setTarget(l *layer) {
	f.mu.Lock()
	f.target = l
	f.mu.Unlock()
}

// pending returns the target layer while a switch to it is waiting for a keyframe.
func (f *forwarder) pending() *layer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.target != nil && f.target != f.current {
		return f.target
	}
	return nil
}

func (f *forwarder) currentLayer() *layer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

// write forwards p, received on layer l, if it belongs to the stream this player is following.
func (f *forwarder) write(l *layer, p *rtp.Packet) error {
	f.mu.Lock()
	if l != f.current {
		if l != f.target || !f.isKeyframe(p.Payload) {
			f.mu.Unlock()
			return nil
		}
		if f.started {
			// Continue from where the previous layer left off: the next sequence number, and a
			// timestamp advanced by the wall time since the last packet sent.
			f.seqOff = f.lastSeq + 1 - p.SequenceNumber
			gap := uint32(f.now().Sub(f.lastAt).Seconds() * float64(f.clockRate))
			f.tsOff = f.lastTS + max(gap, 1) - p.Timestamp
		}
		f.current, f.started = l, true
		f.lastSeq, f.lastTS = p.SequenceNumber+f.seqOff-1, p.Timestamp+f.tsOff
	}
	h := p.Header // copy: the packet is shared between players
	h.SequenceNumber += f.seqOff
	h.Timestamp += f.tsOff
	// Header extension ids were negotiated with the streamer and mean nothing to the player.
	h.Extension, h.Extensions, h.ExtensionProfile = false, nil, 0
	if int16(h.SequenceNumber-f.lastSeq) > 0 {
		f.lastSeq, f.lastTS, f.lastAt = h.SequenceNumber, h.Timestamp, f.now()
	}
	f.mu.Unlock()
	return f.out.WriteRTP(&rtp.Packet{Header: h, Payload: p.Payload})
}
