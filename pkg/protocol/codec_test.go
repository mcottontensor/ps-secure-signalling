package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeLenientNumbers(t *testing.T) {
	m, err := Decode([]byte(`{"type":"disconnectPlayer","playerId":101,"reason":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.(*DisconnectPlayer).PlayerID != "101" {
		t.Fatalf("numeric player id: %+v", m)
	}
	m, err = Decode([]byte(`{"type":"ping","time":1763452800123}`))
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := Encode(&Pong{Time: m.(*Ping).Time}); string(out) != `{"type":"pong","time":1763452800123}` {
		t.Fatalf("pong %s", out)
	}
	m, err = Decode([]byte(`{"type":"layerPreference","spatialLayer":1.0,"temporalLayer":0}`))
	if err != nil || m.(*LayerPreference).SpatialLayer != 1 {
		t.Fatalf("float layer: %v %v", m, err)
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := map[string]error{
		`[]`:                                    ErrNotObject,
		`null`:                                  ErrNotObject,
		`{"type":"nope"}`:                       ErrUnknownType,
		`{"type":5}`:                            ErrInvalid,
		`{"type":"subscribe","streamerId":{}}`:  ErrInvalid,
		`{"type":"endpointId","id":"<script>"}`: ErrInvalid,
		`{"type":"layerPreference","spatialLayer":99,"temporalLayer":0}`:                        ErrInvalid,
		`{"type":"peerDataChannels","sendStreamId":65535,"recvStreamId":1}`:                     ErrInvalid,
		`{"type":"offer","sdp":"` + strings.Repeat("a", MaxSDPBytes+1) + `"}`:                   ErrInvalid,
		`{"type":"iceCandidate","candidate":{"candidate":"` + strings.Repeat("a", 4096) + `"}}`: ErrInvalid,
	}
	for in, want := range cases {
		if _, err := Decode([]byte(in)); !errors.Is(err, want) {
			t.Errorf("%.60s: got %v, want %v", in, err, want)
		}
	}
}

func TestUnknownFieldsDropped(t *testing.T) {
	m, err := Decode([]byte(`{"type":"answer","sdp":"x","playerId":"P","__proto__":{"a":1},"extra":true}`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := Encode(m)
	if string(out) != `{"type":"answer","sdp":"x","playerId":"P"}` {
		t.Fatalf("got %s", out)
	}
}

func TestEveryTypeRoundTrips(t *testing.T) {
	for name, ctor := range registry {
		m := ctor()
		data, err := Encode(m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		back, err := Decode(data)
		if err != nil || TypeOf(back) != name {
			t.Fatalf("%s: %s -> %v %v", name, data, back, err)
		}
	}
}

func TestStreamerIDs(t *testing.T) {
	for _, ok := range []string{"DefaultStreamer", "SFU", "game-1.eu:west", "Ünïcödé 1", "a(b)+c@d"} {
		if !ValidStreamerID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "a\nb", "<b>", "a\"b", strings.Repeat("x", 129), "a\x00"} {
		if ValidStreamerID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
