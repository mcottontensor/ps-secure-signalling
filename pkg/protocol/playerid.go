package protocol

// PlayerIDOf returns the playerId a message carries, and whether its type has one.
func PlayerIDOf(m Message) (PlayerID, bool) {
	switch v := m.(type) {
	case *PlayerConnected:
		return v.PlayerID, true
	case *PlayerDisconnected:
		return v.PlayerID, true
	case *Offer:
		return v.PlayerID, true
	case *Answer:
		return v.PlayerID, true
	case *IceCandidate:
		return v.PlayerID, true
	case *DisconnectPlayer:
		return v.PlayerID, true
	case *LayerPreference:
		return v.PlayerID, true
	case *DataChannelRequest:
		return v.PlayerID, true
	case *PeerDataChannels:
		return v.PlayerID, true
	case *PeerDataChannelsReady:
		return v.PlayerID, true
	case *StreamerDataChannels:
		return v.PlayerID, true
	}
	return "", false
}

// SetPlayerID sets the playerId of a message, reporting false if its type has none.
func SetPlayerID(m Message, id PlayerID) bool {
	switch v := m.(type) {
	case *PlayerConnected:
		v.PlayerID = id
	case *PlayerDisconnected:
		v.PlayerID = id
	case *Offer:
		v.PlayerID = id
	case *Answer:
		v.PlayerID = id
	case *IceCandidate:
		v.PlayerID = id
	case *DisconnectPlayer:
		v.PlayerID = id
	case *LayerPreference:
		v.PlayerID = id
	case *DataChannelRequest:
		v.PlayerID = id
	case *PeerDataChannels:
		v.PlayerID = id
	case *PeerDataChannelsReady:
		v.PlayerID = id
	case *StreamerDataChannels:
		v.PlayerID = id
	default:
		return false
	}
	return true
}
