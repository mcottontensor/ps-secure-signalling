package sfu

import (
	"encoding/binary"
	"errors"
	"unicode/utf16"

	"github.com/pion/webrtc/v4"
)

// Pixel Streaming 2 multiplexes every SFU player's data over one pair of channels to the streamer.
// Each message carries the player id in a header:
//
//	[199][byte length, u16 little endian][player id, UTF-16LE][original message]
//
// and the SFU tells the streamer when a player's channel opens or closes with:
//
//	[198][byte length, u16 little endian][player id, UTF-16LE][1 = open, 0 = closed]
const (
	msgMultiplexed        = 199
	msgChannelRelayStatus = 198

	// Negotiated stream ids on the streamer connection in multiplex mode, from the streamer's
	// point of view: it sends on 0 and receives on 1.
	muxStreamerSendID = 0
	muxStreamerRecvID = 1

	// Negotiated stream ids on a player connection, from the player's point of view. They differ
	// because the frontend only reports readiness for unidirectional channels.
	playerSendID = 0
	playerRecvID = 1

	// Per-player channel ids on a non-multiplexed (PS1) streamer connection are allocated from
	// this range. libwebrtc negotiates 1024 SCTP streams by default; 0 and 1 are left alone.
	firstStreamerChannelID = 2
	lastStreamerChannelID  = 1023

	// maxBufferedBytes is how much may queue on one data channel before messages are dropped,
	// so a peer that stops reading cannot make the SFU buffer without limit.
	maxBufferedBytes = 4 << 20
)

var errBadMultiplexHeader = errors.New("sfu: malformed multiplex header")

func encodePlayerHeader(msgType byte, playerID string, extra int) []byte {
	units := utf16.Encode([]rune(playerID))
	b := make([]byte, 3+2*len(units), 3+2*len(units)+extra)
	b[0] = msgType
	binary.LittleEndian.PutUint16(b[1:], uint16(2*len(units)))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[3+2*i:], u)
	}
	return b
}

// encodeMultiplexed wraps a player's message for the streamer.
func encodeMultiplexed(playerID string, payload []byte) []byte {
	return append(encodePlayerHeader(msgMultiplexed, playerID, len(payload)), payload...)
}

// encodeRelayStatus tells the streamer a player's data channel opened or closed.
func encodeRelayStatus(playerID string, open bool) []byte {
	var flag byte
	if open {
		flag = 1
	}
	return append(encodePlayerHeader(msgChannelRelayStatus, playerID, 1), flag)
}

// decodeMultiplexed splits a message from the streamer into the player it is for and its payload.
func decodeMultiplexed(msg []byte) (playerID string, payload []byte, err error) {
	if len(msg) < 3 || msg[0] != msgMultiplexed {
		return "", nil, errBadMultiplexHeader
	}
	n := int(binary.LittleEndian.Uint16(msg[1:]))
	if n%2 != 0 || 3+n > len(msg) {
		return "", nil, errBadMultiplexHeader
	}
	units := make([]uint16, n/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(msg[3+2*i:])
	}
	return string(utf16.Decode(units)), msg[3+n:], nil
}

// dcMessage is a data channel message waiting to be relayed.
type dcMessage struct {
	data   []byte
	isText bool
}

// sendDC sends on a data channel unless it is not open or too far behind, reporting whether it
// sent. Text and binary are kept apart because the receiving side may treat them differently.
func sendDC(dc *webrtc.DataChannel, m dcMessage) bool {
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen || dc.BufferedAmount() > maxBufferedBytes {
		return false
	}
	var err error
	if m.isText {
		err = dc.SendText(string(m.data))
	} else {
		err = dc.Send(m.data)
	}
	return err == nil
}

// pendingQueue holds a few messages for a channel that is not ready yet. Messages beyond its limits
// are dropped: the data path must never grow without bound for a peer that never becomes ready.
type pendingQueue struct {
	msgs  []dcMessage
	bytes int
}

const (
	maxPendingMessages = 64
	maxPendingBytes    = 1 << 20
)

func (q *pendingQueue) push(m dcMessage) bool {
	if len(q.msgs) >= maxPendingMessages || q.bytes+len(m.data) > maxPendingBytes {
		return false
	}
	q.msgs = append(q.msgs, m)
	q.bytes += len(m.data)
	return true
}

func (q *pendingQueue) drain() []dcMessage {
	m := q.msgs
	q.msgs, q.bytes = nil, 0
	return m
}
