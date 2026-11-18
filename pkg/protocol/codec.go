package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
)

// Field limits applied by Decode. A peer controls the size of everything it sends; nothing larger
// than this has a legitimate use.
const (
	MaxSDPBytes       = 128 << 10
	MaxCandidateBytes = 2 << 10
	MaxIDBytes        = 128
	MaxTextBytes      = 1 << 10
)

var (
	ErrNotObject   = errors.New("protocol: message is not a JSON object")
	ErrUnknownType = errors.New("protocol: unknown message type")
	ErrInvalid     = errors.New("protocol: invalid message")
)

var registry = map[string]func() Message{
	TypeConfig:                func() Message { return &Config{} },
	TypeIdentify:              func() Message { return &Identify{} },
	TypeEndpointID:            func() Message { return &EndpointID{} },
	TypeEndpointIDConfirm:     func() Message { return &EndpointIDConfirm{} },
	TypeStreamerIDChanged:     func() Message { return &StreamerIDChanged{} },
	TypeListStreamers:         func() Message { return &ListStreamers{} },
	TypeStreamerList:          func() Message { return &StreamerList{} },
	TypeSubscribe:             func() Message { return &Subscribe{} },
	TypeUnsubscribe:           func() Message { return &Unsubscribe{} },
	TypeSubscribeFailed:       func() Message { return &SubscribeFailed{} },
	TypePlayerConnected:       func() Message { return &PlayerConnected{} },
	TypePlayerDisconnected:    func() Message { return &PlayerDisconnected{} },
	TypeOffer:                 func() Message { return &Offer{} },
	TypeAnswer:                func() Message { return &Answer{} },
	TypeIceCandidate:          func() Message { return &IceCandidate{} },
	TypeDisconnectPlayer:      func() Message { return &DisconnectPlayer{} },
	TypePing:                  func() Message { return &Ping{} },
	TypePong:                  func() Message { return &Pong{} },
	TypeStreamerDisconnected:  func() Message { return &StreamerDisconnected{} },
	TypeLayerPreference:       func() Message { return &LayerPreference{} },
	TypeDataChannelRequest:    func() Message { return &DataChannelRequest{} },
	TypePeerDataChannels:      func() Message { return &PeerDataChannels{} },
	TypePeerDataChannelsReady: func() Message { return &PeerDataChannelsReady{} },
	TypeStreamerDataChannels:  func() Message { return &StreamerDataChannels{} },
	TypeStartStreaming:        func() Message { return &StartStreaming{} },
	TypeStopStreaming:         func() Message { return &StopStreaming{} },
	TypePlayerCount:           func() Message { return &PlayerCount{} },
	TypeStats:                 func() Message { return &Stats{} },
}

var typeNames = func() map[reflect.Type]string {
	m := make(map[reflect.Type]string, len(registry))
	for name, ctor := range registry {
		m[reflect.TypeOf(ctor())] = name
	}
	return m
}()

// TypeOf returns the wire type name of a message.
func TypeOf(m Message) string { return typeNames[reflect.TypeOf(m)] }

// Decode parses one websocket frame into its typed message and validates it.
func Decode(data []byte) (Message, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil, ErrNotObject
	}
	var h Header
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	ctor, ok := registry[h.Type]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownType, truncate(h.Type, 64))
	}
	m := ctor()
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, h.Type, err)
	}
	if err := Validate(m); err != nil {
		return nil, err
	}
	return m, nil
}

// Encode serialises a message, setting its type field.
func Encode(m Message) ([]byte, error) {
	name := TypeOf(m)
	if name == "" {
		return nil, fmt.Errorf("protocol: %T is not a registered message", m)
	}
	m.header().Type = name
	return json.Marshal(m)
}

// streamerIDPattern is what a streamer may call itself: letters, digits and a little punctuation.
// IDs are shown in player UIs and written to logs, so markup and control characters are refused.
var streamerIDPattern = regexp.MustCompile(`^[\p{L}\p{N}_.:@+()\- ]{1,128}$`)

// ValidStreamerID reports whether id is an acceptable streamer id.
func ValidStreamerID(id string) bool { return streamerIDPattern.MatchString(id) }

// Validate enforces the field limits for messages that carry peer-controlled text.
func Validate(m Message) error {
	check := func(ok bool, what string) error {
		if ok {
			return nil
		}
		return fmt.Errorf("%w: %s: %s", ErrInvalid, TypeOf(m), what)
	}
	idOK := func(p PlayerID) bool { return len(p) <= MaxIDBytes }
	switch v := m.(type) {
	case *EndpointID:
		return check(v.ID == "" || ValidStreamerID(v.ID), "bad id")
	case *Subscribe:
		return check(len(v.StreamerID) <= MaxIDBytes, "streamerId too long")
	case *Offer:
		return check(len(v.SDP) <= MaxSDPBytes && idOK(v.PlayerID) && len(v.ScalabilityMode) <= 16, "field too long")
	case *Answer:
		return check(len(v.SDP) <= MaxSDPBytes && idOK(v.PlayerID), "field too long")
	case *IceCandidate:
		return check(len(v.Candidate.Candidate) <= MaxCandidateBytes && len(v.Candidate.SdpMid) <= MaxIDBytes && idOK(v.PlayerID), "field too long")
	case *DisconnectPlayer:
		return check(idOK(v.PlayerID) && len(v.Reason) <= MaxTextBytes, "field too long")
	case *LayerPreference:
		return check(idOK(v.PlayerID) && v.SpatialLayer >= -1 && v.SpatialLayer < 16 && v.TemporalLayer >= -1 && v.TemporalLayer < 16, "layer out of range")
	case *PeerDataChannels:
		return check(idOK(v.PlayerID) && streamIDOK(v.SendStreamID) && streamIDOK(v.RecvStreamID), "bad stream id")
	case *StreamerDataChannels:
		return check(idOK(v.PlayerID) && idOK(v.SFUID) && streamIDOK(v.SendStreamID) && streamIDOK(v.RecvStreamID), "bad stream id")
	case *Ping:
		return check(len(v.Time) <= 32, "time too long")
	case *Pong:
		return check(len(v.Time) <= 32, "time too long")
	case *Stats:
		return check(len(v.Data) <= MaxSDPBytes, "data too long")
	}
	return nil
}

// SCTP stream ids are 16 bit; 65535 is reserved.
func streamIDOK(i Int) bool { return i >= 0 && i < 65535 }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
