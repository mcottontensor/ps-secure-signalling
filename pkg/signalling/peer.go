package signalling

import (
	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/wsconn"
)

// peer is one connection. A streamer has the streamer half, a player the player half, and an SFU
// both: it subscribes to a streamer like a player and is subscribed to by players like a streamer.
// Every field below conn is guarded by Server.mu.
type peer struct {
	role     auth.Role
	identity *auth.Identity
	remote   string
	conn     *wsconn.Conn

	// Streamer half.
	streamerID  string
	streaming   bool             // identified and ready to be subscribed to
	subscribers map[string]*peer // by player id

	// Player half.
	playerID string
	upstream *peer // the streamer this peer is subscribed to
}

func (p *peer) isPlayer() bool   { return p.role == auth.RolePlayer || p.role == auth.RoleSFU }
func (p *peer) isStreamer() bool { return p.role == auth.RoleStreamer || p.role == auth.RoleSFU }

func (p *peer) infoLocked() PeerInfo {
	return PeerInfo{
		Role:       p.role,
		Identity:   p.identity,
		RemoteAddr: p.remote,
		PlayerID:   p.playerID,
		StreamerID: p.streamerID,
	}
}
