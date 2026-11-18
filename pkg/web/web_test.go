package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/signalling"
)

func TestStatic(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "www")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "css"), 0o755))
	must(os.WriteFile(filepath.Join(root, "player.html"), []byte("home"), 0o644))
	must(os.WriteFile(filepath.Join(root, "css", "a.css"), []byte("css"), 0o644))
	must(os.WriteFile(filepath.Join(root, ".env"), []byte("secret"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "outside.txt"), []byte("secret"), 0o644))
	if err := os.Symlink(filepath.Join(dir, "outside.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	h, err := Static(root, "player.html")
	must(err)
	get := func(method, path string) (int, string) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/", nil)
		r.URL.Path = path // bypass client-side cleaning, as a hostile client would
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}
	if code, body := get("GET", "/"); code != 200 || body != "home" {
		t.Fatalf("homepage: %d %q", code, body)
	}
	if code, _ := get("GET", "/css/a.css"); code != 200 {
		t.Fatalf("asset: %d", code)
	}
	for _, p := range []string{"/.env", "/css/", "/../outside.txt", "/link.txt", "/css/../../outside.txt"} {
		if code, body := get("GET", p); code == 200 || body == "secret" {
			t.Errorf("%s served: %d %q", p, code, body)
		}
	}
	if code, _ := get("POST", "/"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}
}

func TestClientIP(t *testing.T) {
	ip := ClientIPFunc([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 1.2.3.4, 10.0.0.2")

	r.RemoteAddr = "203.0.113.9:1234" // untrusted peer: header ignored
	if got := ip(r); got != "203.0.113.9" {
		t.Errorf("untrusted: %s", got)
	}
	r.RemoteAddr = "10.0.0.1:1234" // trusted proxy: first untrusted hop from the right
	if got := ip(r); got != "1.2.3.4" {
		t.Errorf("trusted: %s", got)
	}
}

func TestAPIRequiresAdmin(t *testing.T) {
	st, _ := auth.NewStaticTokens(auth.TokenSource{}, []auth.StaticToken{
		{Token: "admin-0123456789abcdef", Roles: []auth.Role{auth.RoleAdmin}},
		{Token: "player-0123456789abcd", Roles: []auth.Role{auth.RolePlayer}},
	})
	chain := &auth.Chain{Authenticators: []auth.Authenticator{st}, Anonymous: []auth.Role{auth.RolePlayer}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := API(signalling.New(signalling.Config{Logger: log}, chain, signalling.Hooks{}), chain, "test", log)

	for tok, want := range map[string]int{"": 401, "player-0123456789abcd": 401, "admin-0123456789abcdef": 200} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/status", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("token %q: %d", tok, rec.Code)
		}
	}
	// The API never reads credentials from the URL.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status?token=admin-0123456789abcdef", nil))
	if rec.Code != 401 {
		t.Errorf("query token accepted: %d", rec.Code)
	}
}
