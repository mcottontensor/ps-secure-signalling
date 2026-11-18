// Command embed shows the signalling server used as a library: a custom authenticator, hooks for
// multi-tenant isolation and auditing, and the handlers mounted on an application's own mux.
//
// Each tenant's API key lets its streamers publish, and its players watch, only streamer ids
// prefixed with the tenant name, so tenants sharing one server can neither see nor reach each other.
//
//	go run ./examples/embed
//	streamer: ws://127.0.0.1:8888/?token=acme-key-0123456789   (registers as "acme/<requested id>")
//	player:   ws://127.0.0.1:8080/?token=acme-key-0123456789
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/signalling"
)

// tenants maps API keys to tenant names. A real application would look these up in a database.
var tenants = map[string]string{
	"acme-key-0123456789":   "acme",
	"globex-key-0123456789": "globex",
}

// tenantAuth is a custom Authenticator: any type with this one method can join an auth.Chain.
// Streamers scopes what the identity may list and subscribe to; patterns are allowed.
type tenantAuth struct{}

func (tenantAuth) Authenticate(_ context.Context, r *http.Request, role auth.Role) (*auth.Identity, error) {
	for _, tok := range auth.DefaultTokenSource.Tokens(r) {
		if tenant, ok := tenants[tok]; ok {
			return &auth.Identity{
				Subject:   tenant,
				Method:    "tenant-key",
				Roles:     []auth.Role{auth.RoleStreamer, auth.RolePlayer},
				Streamers: []string{tenant + "/*"},
			}, nil
		}
	}
	return nil, auth.ErrNoCredentials
}

func main() {
	log := slog.Default()
	hooks := signalling.Hooks{
		// Streamers register under their tenant's prefix, whatever they ask for.
		AuthorizeStreamerID: func(info signalling.PeerInfo, requested string) (string, error) {
			prefix := info.Identity.Subject + "/"
			if requested == "" {
				requested = "default"
			}
			return prefix + strings.TrimPrefix(requested, prefix), nil
		},
		// Refuse new connections from a tenant that is over its quota (a stub here).
		OnConnect: func(r *http.Request, info signalling.PeerInfo) error {
			if overQuota(info.Identity.Subject) {
				return errors.New("tenant over quota")
			}
			return nil
		},
		// An audit trail, and a policy that drops a message type outright.
		OnMessage: func(info signalling.PeerInfo, m protocol.Message) bool {
			log.Debug("message", "tenant", info.Identity.Subject, "role", info.Role, "type", protocol.TypeOf(m))
			_, isStats := m.(*protocol.Stats)
			return !isStats
		},
		OnDisconnect: func(info signalling.PeerInfo) {
			log.Info("left", "tenant", info.Identity.Subject, "player", info.PlayerID, "streamer", info.StreamerID)
		},
	}

	authn := &auth.Chain{Authenticators: []auth.Authenticator{tenantAuth{}}}
	srv := signalling.New(signalling.Config{MaxSubscribers: 4, Logger: log}, authn, hooks)

	// The handlers are plain http.Handlers: wrap them in any middleware, mount them anywhere.
	players := http.NewServeMux()
	players.Handle("/", srv.Handler(auth.RolePlayer))
	players.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	go func() {
		log.Error("streamers", "err", http.ListenAndServe("127.0.0.1:8888", srv.Handler(auth.RoleStreamer)))
	}()
	log.Error("players", "err", http.ListenAndServe("127.0.0.1:8080", players))
}

func overQuota(string) bool { return false }
