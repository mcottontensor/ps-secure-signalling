package sfu

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestKeyframeDetection(t *testing.T) {
	cases := []struct {
		name string
		f    keyframeFunc
		in   []byte
		want bool
	}{
		{"vp8 key", isVP8Keyframe, []byte{0x10, 0x00}, true},
		{"vp8 delta", isVP8Keyframe, []byte{0x10, 0x01}, false},
		{"vp8 not start", isVP8Keyframe, []byte{0x00, 0x00}, false},
		{"vp8 partition 1", isVP8Keyframe, []byte{0x11, 0x00}, false},
		{"vp8 ext 15-bit picture id key", isVP8Keyframe, []byte{0x90, 0x80, 0x81, 0x23, 0x00}, true},
		{"vp8 ext picture id + tl0 + tid delta", isVP8Keyframe, []byte{0x90, 0xe0, 0x05, 0x01, 0x40, 0x01}, false},
		{"vp8 truncated", isVP8Keyframe, []byte{0x90, 0x80}, false},
		{"vp8 empty", isVP8Keyframe, nil, false},
		{"vp9 key no layers", isVP9Keyframe, []byte{0x08}, true},
		{"vp9 inter", isVP9Keyframe, []byte{0x48}, false},
		{"vp9 not begin", isVP9Keyframe, []byte{0x00}, false},
		{"vp9 key sid0 with picture id", isVP9Keyframe, []byte{0xa8, 0x81, 0x02, 0x00}, true},
		{"vp9 key sid1", isVP9Keyframe, []byte{0x28, 0x02}, false},
		{"vp9 truncated", isVP9Keyframe, []byte{0xa8}, false},
		{"h264 idr", isH264Keyframe, []byte{0x65, 0x88}, true},
		{"h264 sps", isH264Keyframe, []byte{0x67, 0x42}, true},
		{"h264 non-idr", isH264Keyframe, []byte{0x41, 0x9a}, false},
		{"h264 stap-a sps", isH264Keyframe, []byte{0x78, 0x00, 0x02, 0x67, 0x42, 0x00, 0x02, 0x68, 0xce}, true},
		{"h264 stap-a no key", isH264Keyframe, []byte{0x78, 0x00, 0x02, 0x41, 0x42}, false},
		{"h264 stap-a bad size", isH264Keyframe, []byte{0x78, 0x00, 0x09, 0x67}, false},
		{"h264 fu-a idr start", isH264Keyframe, []byte{0x7c, 0x85, 0x00}, true},
		{"h264 fu-a idr middle", isH264Keyframe, []byte{0x7c, 0x05, 0x00}, false},
		{"av1 new sequence", isAV1Keyframe, []byte{0x18}, true},
		{"av1 continuation", isAV1Keyframe, []byte{0x88}, false},
	}
	for _, c := range cases {
		if got := c.f(c.in); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestMultiplexHeader(t *testing.T) {
	for _, id := range []string{"Player0", "", "Spieler-ü-🎮"} {
		msg := encodeMultiplexed(id, []byte{1, 2, 3})
		gotID, payload, err := decodeMultiplexed(msg)
		if err != nil || gotID != id || !bytes.Equal(payload, []byte{1, 2, 3}) {
			t.Fatalf("round trip %q: got %q %v %v", id, gotID, payload, err)
		}
	}
	// Layout is fixed by the plugin: type, byte length (LE), UTF-16LE id.
	if got := encodeMultiplexed("P1", []byte{9}); !bytes.Equal(got, []byte{199, 4, 0, 'P', 0, '1', 0, 9}) {
		t.Fatalf("layout: %v", got)
	}
	if got := encodeRelayStatus("P", true); !bytes.Equal(got, []byte{198, 2, 0, 'P', 0, 1}) {
		t.Fatalf("relay status: %v", got)
	}
	for _, bad := range [][]byte{nil, {199}, {199, 1}, {198, 0, 0}, {199, 3, 0, 1, 2, 3}, {199, 8, 0, 1, 2}} {
		if _, _, err := decodeMultiplexed(bad); err == nil {
			t.Errorf("accepted malformed %v", bad)
		}
	}
}

func TestRidOrder(t *testing.T) {
	mk := func(rids ...string) []*layer {
		var ls []*layer
		for _, r := range rids {
			ls = append(ls, &layer{rid: r})
		}
		return ls
	}
	for _, c := range []struct{ in, want []string }{
		{[]string{"simulcast2", "simulcast0", "simulcast1"}, []string{"simulcast0", "simulcast1", "simulcast2"}}, // PS2
		{[]string{"simulcast3", "simulcast2", "simulcast1"}, []string{"simulcast1", "simulcast2", "simulcast3"}}, // PS1
		{[]string{""}, []string{""}},
	} {
		ls := mk(c.in...)
		ridOrder(ls)
		for i := range ls {
			if ls[i].rid != c.want[i] {
				t.Fatalf("%v: got %v at %d", c.in, ls[i].rid, i)
			}
		}
	}
}

type capture struct{ pkts []rtp.Packet }

func (c *capture) WriteRTP(p *rtp.Packet) error { c.pkts = append(c.pkts, *p); return nil }

func TestForwarderSwitchKeepsStreamContinuous(t *testing.T) {
	out := &capture{}
	f := newForwarder(out, webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}})
	now := time.Unix(1000, 0)
	f.now = func() time.Time { return now }
	big, small := &layer{rid: "simulcast0"}, &layer{rid: "simulcast1"}
	key, delta := []byte{0x10, 0x00}, []byte{0x10, 0x01}
	pkt := func(seq uint16, ts uint32, payload []byte) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, Extension: true,
			ExtensionProfile: 0xBEDE}, Payload: payload}
	}

	f.setTarget(big)
	_ = f.write(big, pkt(100, 5000, delta)) // no keyframe yet: dropped
	_ = f.write(small, pkt(7, 900, key))    // not the target: dropped
	_ = f.write(big, pkt(101, 5000, key))
	_ = f.write(big, pkt(102, 8000, delta))
	if len(out.pkts) != 2 || out.pkts[0].SequenceNumber != 101 || out.pkts[0].Extension {
		t.Fatalf("start: %+v", out.pkts)
	}

	f.setTarget(small)
	_ = f.write(small, pkt(8, 1000, delta)) // waits for a keyframe on the new layer
	_ = f.write(big, pkt(103, 8000, delta)) // old layer still flows meanwhile
	now = now.Add(33 * time.Millisecond)
	if f.pending() != small {
		t.Fatal("switch should be pending")
	}
	_ = f.write(small, pkt(9, 4000, key))
	_ = f.write(big, pkt(104, 11000, delta)) // old layer is now dropped
	_ = f.write(small, pkt(10, 7000, delta))

	got := out.pkts
	if len(got) != 5 {
		t.Fatalf("got %d packets", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].SequenceNumber != got[i-1].SequenceNumber+1 {
			t.Fatalf("sequence gap at %d: %d -> %d", i, got[i-1].SequenceNumber, got[i].SequenceNumber)
		}
	}
	if d := got[3].Timestamp - got[2].Timestamp; d != 33*90 {
		t.Fatalf("timestamp advanced %d across switch, want %d", d, 33*90)
	}
	if d := got[4].Timestamp - got[3].Timestamp; d != 3000 {
		t.Fatalf("timestamp delta after switch %d", d)
	}
	if f.pending() != nil || f.currentLayer() != small {
		t.Fatal("switch should be complete")
	}
}

func TestLayerSelector(t *testing.T) {
	rates := []int64{8_000_000, 2_000_000, 500_000}
	now := time.Unix(0, 0)
	s := newLayerSelector(0)
	step := func(est int) int { now = now.Add(selectInterval); return s.next(now, rates, est) }

	if got := step(0); got != 0 {
		t.Fatalf("no estimate should use the ceiling, got %d", got)
	}
	if got := step(20_000_000); got != 0 {
		t.Fatalf("ample bandwidth: got %d", got)
	}
	step(3_000_000) // one low sample is not enough
	if s.current != 0 {
		t.Fatal("stepped down too eagerly")
	}
	if got := step(3_000_000); got != 1 {
		t.Fatalf("should step down to 1, got %d", got)
	}
	if got := step(400_000); got != 1 {
		t.Fatal("hysteresis")
	}
	if got := step(400_000); got != 2 {
		t.Fatalf("should drop to smallest, got %d", got)
	}
	// The estimate stays near what is sent; a speculative probe goes up one layer after backoff.
	for range int(probeBackoffMin / selectInterval) {
		step(700_000)
	}
	if s.current != 1 || !s.probing {
		t.Fatalf("expected a probe to layer 1, at %d probing=%v", s.current, s.probing)
	}
	// A probe that the estimate does not follow fails and doubles the backoff.
	step(700_000)
	step(700_000)
	if s.current != 2 || s.backoff != 2*probeBackoffMin {
		t.Fatalf("failed probe: at %d backoff %v", s.current, s.backoff)
	}
	// A ceiling below the current layer applies at once, and is clamped to the layers that exist.
	s.ceiling = 9
	if got := step(20_000_000); got != 2 {
		t.Fatalf("ceiling clamp: got %d", got)
	}
	if clampLayer(-1, 3) != 0 || clampLayer(5, 3) != 2 || clampLayer(1, 0) != 0 {
		t.Fatal("clampLayer")
	}
}

func TestPendingQueueBounded(t *testing.T) {
	var q pendingQueue
	for range maxPendingMessages {
		if !q.push(dcMessage{data: []byte{1}}) {
			t.Fatal("rejected within limit")
		}
	}
	if q.push(dcMessage{data: []byte{1}}) {
		t.Fatal("accepted beyond message limit")
	}
	q.drain()
	if q.push(dcMessage{data: make([]byte, maxPendingBytes+1)}) {
		t.Fatal("accepted beyond byte limit")
	}
}
