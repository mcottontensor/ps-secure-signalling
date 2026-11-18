package web

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/signalling"
)

// API serves the read-only REST API under /api/. Every request must authenticate with the admin
// role; the API reveals client addresses and identities, so it is never anonymous.
//
//	GET /api/status            uptime, counts, version
//	GET /api/streamers         all streamers, with their subscribers
//	GET /api/streamers/{id}
//	GET /api/players           all players
//	GET /api/players/{id}
func API(srv *signalling.Server, authn auth.Authenticator, version string, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		snap := srv.Snapshot()
		writeJSON(w, map[string]any{
			"uptime":           snap.Uptime.Milliseconds(),
			"streamer_count":   len(snap.Streamers),
			"player_count":     len(snap.Players),
			"version":          version,
			"protocol_version": protocol.Version,
		})
	})
	mux.HandleFunc("GET /api/streamers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, srv.Snapshot().Streamers)
	})
	mux.HandleFunc("GET /api/streamers/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, s := range srv.Snapshot().Streamers {
			if s.StreamerID == r.PathValue("id") {
				writeJSON(w, s)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /api/players", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, srv.Snapshot().Players)
	})
	mux.HandleFunc("GET /api/players/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, p := range srv.Snapshot().Players {
			if p.PlayerID == r.PathValue("id") {
				writeJSON(w, p)
				return
			}
		}
		http.NotFound(w, r)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := authn.Authenticate(r.Context(), r, auth.RoleAdmin)
		if err != nil {
			log.Info("api request refused", "path", r.URL.Path, "reason", err)
			w.Header().Set("WWW-Authenticate", `Bearer realm="pixelstreaming"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		log.Debug("api request", "path", r.URL.Path, "subject", id.Subject)
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
