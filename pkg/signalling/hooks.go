package signalling

import (
	"encoding/json"
	"net/http"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
)

// PeerInfo describes a connection to hooks and the REST API. It is a snapshot; it does not change
// after it is handed out.
type PeerInfo struct {
	Role       auth.Role      `json:"role"`
	Identity   *auth.Identity `json:"identity"`
	RemoteAddr string         `json:"remoteAddress"`
	// PlayerID is set for players and SFUs.
	PlayerID string `json:"playerId,omitempty"`
	// StreamerID is set for streamers and SFUs. Before a streamer identifies itself it holds a
	// placeholder id.
	StreamerID string `json:"streamerId,omitempty"`
}

// Hooks let an embedding application observe and steer the server without changing it. Every hook
// is optional. Hooks are called without the server's lock held, from the goroutine of the
// connection concerned, so they may block that connection (and only that connection) and may call
// back into the Server.
type Hooks struct {
	// OnConnect runs after authentication and before the websocket upgrade. Returning an error
	// refuses the connection with 403. info.PlayerID and info.StreamerID are not yet assigned.
	OnConnect func(r *http.Request, info PeerInfo) error

	// OnRegistered runs once the connection has its ids and has been sent its config.
	OnRegistered func(info PeerInfo)

	// OnDisconnect runs once the connection has gone and its subscriptions have been undone.
	OnDisconnect func(info PeerInfo)

	// OnMessage sees each message from a peer after validation and before it is acted on.
	// Returning false drops it. The message may be modified.
	OnMessage func(info PeerInfo, m protocol.Message) bool

	// AuthorizeSubscribe decides whether a player or SFU may subscribe to a streamer, after the
	// identity's own Streamers scope has allowed it. Returning an error refuses with subscribeFailed.
	AuthorizeSubscribe func(info PeerInfo, streamerID string) error

	// AuthorizeStreamerID decides the id a streamer or SFU registers as, after the identity's own
	// PublishAs scope has allowed it. Return the id to use (requested, or a replacement such as a
	// tenant-prefixed one), or an error to disconnect the streamer.
	AuthorizeStreamerID func(info PeerInfo, requested string) (string, error)

	// PeerOptions returns the peerConnectionOptions for one connection's config message, replacing
	// the static ones. Per-connection TURN credentials are the reason this exists.
	PeerOptions func(info PeerInfo) json.RawMessage
}
