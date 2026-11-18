// Package protocol defines the Pixel Streaming signalling messages (protocol 1.3.0) exactly as they
// travel on the wire: JSON objects carrying a "type" field. The authoritative description is
// Common/protobuf/signalling_messages.proto in PixelStreamingInfrastructure.
//
// Every message is decoded into a typed struct and re-encoded from it, so fields that are not part
// of the protocol are dropped rather than forwarded between peers.
package protocol

import (
	"encoding/json"
	"strconv"
)

// Version is the signalling protocol version this implementation speaks.
const Version = "1.3.0"

// Message types.
const (
	TypeConfig                = "config"
	TypeIdentify              = "identify"
	TypeEndpointID            = "endpointId"
	TypeEndpointIDConfirm     = "endpointIdConfirm"
	TypeStreamerIDChanged     = "streamerIdChanged"
	TypeListStreamers         = "listStreamers"
	TypeStreamerList          = "streamerList"
	TypeSubscribe             = "subscribe"
	TypeUnsubscribe           = "unsubscribe"
	TypeSubscribeFailed       = "subscribeFailed"
	TypePlayerConnected       = "playerConnected"
	TypePlayerDisconnected    = "playerDisconnected"
	TypeOffer                 = "offer"
	TypeAnswer                = "answer"
	TypeIceCandidate          = "iceCandidate"
	TypeDisconnectPlayer      = "disconnectPlayer"
	TypePing                  = "ping"
	TypePong                  = "pong"
	TypeStreamerDisconnected  = "streamerDisconnected"
	TypeLayerPreference       = "layerPreference"
	TypeDataChannelRequest    = "dataChannelRequest"
	TypePeerDataChannels      = "peerDataChannels"
	TypePeerDataChannelsReady = "peerDataChannelsReady"
	TypeStreamerDataChannels  = "streamerDataChannels"
	TypeStartStreaming        = "startStreaming"
	TypeStopStreaming         = "stopStreaming"
	TypePlayerCount           = "playerCount"
	TypeStats                 = "stats"
)

// Message is implemented by every signalling message. Embed Header to implement it.
type Message interface{ header() *Header }

// Header is the field every message carries. Encode fills it in, so callers never set it.
type Header struct {
	Type string `json:"type"`
}

func (h *Header) header() *Header { return h }

// PlayerID is a player id. Older streamers send numeric ids, so it decodes from a JSON string or
// number, and always encodes as a string.
type PlayerID string

func (p *PlayerID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*p = PlayerID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*p = PlayerID(n.String())
	return nil
}

// Number is a JSON number echoed exactly as received. The proto says int32, but browsers send
// millisecond timestamps in ping, which do not fit.
type Number = json.Number

// Int is an integer field. It accepts any JSON number, as JavaScript peers do not distinguish.
type Int int64

func (i *Int) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	if v, err := n.Int64(); err == nil {
		*i = Int(v)
		return nil
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return err
	}
	*i = Int(f)
	return nil
}

type Config struct {
	Header
	PeerConnectionOptions json.RawMessage `json:"peerConnectionOptions,omitempty"`
	ProtocolVersion       string          `json:"protocolVersion,omitempty"`
}

type Identify struct{ Header }

type EndpointID struct {
	Header
	ID              string `json:"id"`
	ProtocolVersion string `json:"protocolVersion,omitempty"`
}

type EndpointIDConfirm struct {
	Header
	CommittedID string `json:"committedId"`
}

type StreamerIDChanged struct {
	Header
	NewID string `json:"newID"`
}

type ListStreamers struct{ Header }

type StreamerList struct {
	Header
	IDs []string `json:"ids"`
}

type Subscribe struct {
	Header
	StreamerID string `json:"streamerId"`
}

type Unsubscribe struct{ Header }

type SubscribeFailed struct {
	Header
	Message string `json:"message"`
}

type PlayerConnected struct {
	Header
	DataChannel bool     `json:"dataChannel"`
	SFU         bool     `json:"sfu"`
	PlayerID    PlayerID `json:"playerId"`
}

type PlayerDisconnected struct {
	Header
	PlayerID PlayerID `json:"playerId"`
}

type Offer struct {
	Header
	SDP             string   `json:"sdp"`
	PlayerID        PlayerID `json:"playerId,omitempty"`
	SFU             *bool    `json:"sfu,omitempty"`
	Multiplex       *bool    `json:"multiplex,omitempty"`
	ScalabilityMode string   `json:"scalabilityMode,omitempty"`
}

type Answer struct {
	Header
	SDP           string   `json:"sdp"`
	PlayerID      PlayerID `json:"playerId,omitempty"`
	MinBitrateBps *Int     `json:"minBitrateBps,omitempty"`
	MaxBitrateBps *Int     `json:"maxBitrateBps,omitempty"`
}

type IceCandidateData struct {
	Candidate        string  `json:"candidate"`
	SdpMid           string  `json:"sdpMid"`
	SdpMLineIndex    Int     `json:"sdpMLineIndex"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

type IceCandidate struct {
	Header
	Candidate IceCandidateData `json:"candidate"`
	PlayerID  PlayerID         `json:"playerId,omitempty"`
}

type DisconnectPlayer struct {
	Header
	PlayerID PlayerID `json:"playerId"`
	Reason   string   `json:"reason,omitempty"`
}

type Ping struct {
	Header
	Time Number `json:"time,omitempty"`
}

type Pong struct {
	Header
	Time Number `json:"time,omitempty"`
}

type StreamerDisconnected struct{ Header }

type LayerPreference struct {
	Header
	SpatialLayer  Int      `json:"spatialLayer"`
	TemporalLayer Int      `json:"temporalLayer"`
	PlayerID      PlayerID `json:"playerId,omitempty"`
}

type DataChannelRequest struct {
	Header
	PlayerID PlayerID `json:"playerId,omitempty"`
}

type PeerDataChannels struct {
	Header
	PlayerID     PlayerID `json:"playerId,omitempty"`
	SendStreamID Int      `json:"sendStreamId"`
	RecvStreamID Int      `json:"recvStreamId"`
}

type PeerDataChannelsReady struct {
	Header
	PlayerID PlayerID `json:"playerId,omitempty"`
}

type StreamerDataChannels struct {
	Header
	SFUID        PlayerID `json:"sfuId"`
	PlayerID     PlayerID `json:"playerId,omitempty"`
	SendStreamID Int      `json:"sendStreamId"`
	RecvStreamID Int      `json:"recvStreamId"`
}

type StartStreaming struct{ Header }

type StopStreaming struct{ Header }

type PlayerCount struct {
	Header
	Count Int `json:"count"`
}

type Stats struct {
	Header
	Data string `json:"data"`
}
