package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
)

var hmacSecret = []byte("0123456789abcdef0123456789abcdef")

func req(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func sign(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims, kid string) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTHMAC(t *testing.T) {
	j, err := auth.NewJWT(context.Background(), auth.DefaultTokenSource, auth.JWTConfig{HMACSecret: hmacSecret, Audience: "ps"})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).Unix()
	good := sign(t, jwt.SigningMethodHS256, hmacSecret, jwt.MapClaims{
		"sub": "alice", "aud": "ps", "exp": exp, "ps_roles": []string{"player"}, "ps_streamers": "Game Other",
	}, "")
	id, err := j.Authenticate(context.Background(), req(good), auth.RolePlayer)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "alice" || !id.HasRole(auth.RolePlayer) || len(id.Streamers) != 2 || id.ExpiresAt.Unix() != exp {
		t.Fatalf("identity %+v", id)
	}

	bad := map[string]string{
		"expired":   sign(t, jwt.SigningMethodHS256, hmacSecret, jwt.MapClaims{"aud": "ps", "exp": time.Now().Add(-time.Hour).Unix()}, ""),
		"no exp":    sign(t, jwt.SigningMethodHS256, hmacSecret, jwt.MapClaims{"aud": "ps"}, ""),
		"wrong aud": sign(t, jwt.SigningMethodHS256, hmacSecret, jwt.MapClaims{"aud": "other", "exp": exp}, ""),
		"wrong key": sign(t, jwt.SigningMethodHS256, []byte("another-secret-another-secret-xx"), jwt.MapClaims{"aud": "ps", "exp": exp}, ""),
		"alg none":  sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, jwt.MapClaims{"aud": "ps", "exp": exp}, ""),
		"HS512":     sign(t, jwt.SigningMethodHS512, hmacSecret, jwt.MapClaims{"aud": "ps", "exp": exp}, ""),
	}
	for name, tok := range bad {
		if _, err := j.Authenticate(context.Background(), req(tok), auth.RolePlayer); !errors.Is(err, auth.ErrUnauthorized) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := j.Authenticate(context.Background(), req("not-a-jwt"), auth.RolePlayer); !errors.Is(err, auth.ErrNoCredentials) {
		t.Errorf("non-JWT: got %v", err)
	}
}

func TestJWTJWKS(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwks := map[string]any{"keys": []map[string]any{{
		"kty": "EC", "crv": "P-256", "kid": "k1", "alg": "ES256", "use": "sig",
		"x": base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		"y": base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}}}
	var issuer string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(jwks)
		}
	}))
	defer ts.Close()
	issuer = ts.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j, err := auth.NewJWT(ctx, auth.DefaultTokenSource, auth.JWTConfig{OIDCIssuer: issuer})
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{"iss": issuer, "sub": "ue1", "exp": time.Now().Add(time.Hour).Unix(), "ps_roles": "streamer", "ps_publish": []string{"Game"}}
	id, err := j.Authenticate(ctx, req(sign(t, jwt.SigningMethodES256, key, claims, "k1")), auth.RoleStreamer)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Pinned() || !id.MayPublishAs("Game") || id.MayPublishAs("Other") {
		t.Fatalf("identity %+v", id)
	}
	// An HMAC token signed with anything must not be accepted by a public-key-only verifier.
	hs := sign(t, jwt.SigningMethodHS256, hmacSecret, claims, "k1")
	if _, err := j.Authenticate(ctx, req(hs), auth.RoleStreamer); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("hmac token accepted: %v", err)
	}
}

func TestChain(t *testing.T) {
	st, _ := auth.NewStaticTokens(auth.DefaultTokenSource, []auth.StaticToken{{Token: "admin-0123456789abcdef", Roles: []auth.Role{auth.RoleAdmin}}})
	j, _ := auth.NewJWT(context.Background(), auth.DefaultTokenSource, auth.JWTConfig{HMACSecret: hmacSecret})
	c := &auth.Chain{Authenticators: []auth.Authenticator{st, j}, Anonymous: []auth.Role{auth.RolePlayer}}
	ctx := context.Background()

	if id, err := c.Authenticate(ctx, req(""), auth.RolePlayer); err != nil || id.Method != "anonymous" {
		t.Fatalf("anonymous player: %v %v", id, err)
	}
	if _, err := c.Authenticate(ctx, req(""), auth.RoleStreamer); err == nil {
		t.Fatal("anonymous streamer accepted")
	}
	// A JWT passes even though the static authenticator rejects it.
	tok := sign(t, jwt.SigningMethodHS256, hmacSecret, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix(), "ps_roles": "streamer"}, "")
	if id, err := c.Authenticate(ctx, req(tok), auth.RoleStreamer); err != nil || id.Method != "jwt" {
		t.Fatalf("jwt streamer: %v %v", id, err)
	}
	// A valid credential without the role is refused.
	if _, err := c.Authenticate(ctx, req("admin-0123456789abcdef"), auth.RoleStreamer); err == nil {
		t.Fatal("admin token accepted as streamer")
	}
}

func TestTokenSources(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?token=q", nil)
	r.Header.Set("Authorization", "bearer h")
	r.AddCookie(&http.Cookie{Name: "ps", Value: "c"})
	got := auth.TokenSource{Query: "token", Cookie: "ps"}.Tokens(r)
	if len(got) != 3 || got[0] != "h" || got[1] != "q" || got[2] != "c" {
		t.Fatalf("tokens %v", got)
	}
	if got := (auth.TokenSource{}).Tokens(r); len(got) != 1 {
		t.Fatalf("header-only tokens %v", got)
	}
}

func TestWebhook(t *testing.T) {
	secret := []byte("webhook-secret")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		if r.Header.Get("X-PS-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
		var in auth.WebhookRequest
		_ = json.Unmarshal(body, &in)
		allow := len(in.Tokens) == 1 && in.Tokens[0] == "letmein"
		_ = json.NewEncoder(w).Encode(auth.WebhookResponse{Allow: allow, Subject: "bob", Streamers: []string{"Game"}})
	}))
	defer ts.Close()

	wh := &auth.Webhook{URL: ts.URL, Secret: secret, Source: auth.DefaultTokenSource}
	id, err := wh.Authenticate(context.Background(), req("letmein"), auth.RolePlayer)
	if err != nil || id.Subject != "bob" || !id.HasRole(auth.RolePlayer) {
		t.Fatalf("allow: %+v %v", id, err)
	}
	if _, err := wh.Authenticate(context.Background(), req("nope"), auth.RolePlayer); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("deny: %v", err)
	}
	wh.Secret = []byte("wrong")
	if _, err := wh.Authenticate(context.Background(), req("letmein"), auth.RolePlayer); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("bad signature: %v", err)
	}
	wh.URL = "http://127.0.0.1:1"
	if _, err := wh.Authenticate(context.Background(), req("letmein"), auth.RolePlayer); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("unreachable webhook must fail closed: %v", err)
	}
}

func TestMTLS(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Game"}, NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)

	m := &auth.MTLS{Roles: []auth.Role{auth.RoleStreamer}, PublishAsSubject: true, AllowedSubjects: []string{"Game"}}
	r := req("")
	if _, err := m.Authenticate(context.Background(), r, auth.RoleStreamer); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("no tls: %v", err)
	}
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	id, err := m.Authenticate(context.Background(), r, auth.RoleStreamer)
	if err != nil || id.Subject != "Game" || !id.MayPublishAs("Game") || id.MayPublishAs("Other") {
		t.Fatalf("%+v %v", id, err)
	}
	m.AllowedSubjects = []string{"Someone"}
	if _, err := m.Authenticate(context.Background(), r, auth.RoleStreamer); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("subject not allowed: %v", err)
	}
	// Presented but unverified certificates do not count.
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if _, err := m.Authenticate(context.Background(), r, auth.RoleStreamer); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("unverified: %v", err)
	}
}

func TestStaticTokenTooShort(t *testing.T) {
	if _, err := auth.NewStaticTokens(auth.DefaultTokenSource, []auth.StaticToken{{Token: "short", Roles: []auth.Role{auth.RolePlayer}}}); err == nil {
		t.Fatal("short token accepted")
	}
}
