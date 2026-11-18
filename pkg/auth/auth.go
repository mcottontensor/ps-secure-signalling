// Package auth decides who is connecting. An Authenticator inspects the HTTP request that opens a
// connection (headers, query, TLS state) and returns an Identity, or says it has nothing to go on.
// A Chain runs several and applies the anonymous-access policy for the role being requested.
//
// To add a scheme of your own, implement Authenticator and put it in the Chain.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Role is what a connection is allowed to be. Each listener asks for exactly one.
type Role string

const (
	RoleStreamer Role = "streamer" // a UE instance producing a stream
	RolePlayer   Role = "player"   // a browser or other client consuming one
	RoleSFU      Role = "sfu"      // a forwarding unit: a player upstream and a streamer downstream
	RoleAdmin    Role = "admin"    // the REST API
)

// Wildcard in Streamers or PublishAs means any streamer id.
const Wildcard = "*"

// Identity is the outcome of authentication. It travels with the connection for its lifetime, and
// is what authorization decisions are made against.
type Identity struct {
	Subject string `json:"subject"`
	Method  string `json:"method"` // which authenticator produced it: token, jwt, mtls, webhook, anonymous
	Roles   []Role `json:"roles"`
	// Streamers lists the streamer ids this identity may subscribe to (players, SFUs) or see in a
	// streamer list. Empty means no restriction.
	Streamers []string `json:"streamers,omitempty"`
	// PublishAs lists the streamer ids this identity may register as (streamers, SFUs). Empty means
	// no restriction, which keeps the legacy behaviour of suffixing an id that is already taken.
	// When set, the id is reserved for this identity: it is never suffixed, and a new connection
	// with the same identity replaces the old one.
	PublishAs []string `json:"publishAs,omitempty"`
	// ExpiresAt is when the credential stops being valid. Zero means it does not expire.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// Claims holds whatever else the authenticator learned, for custom hooks to use.
	Claims map[string]any `json:"-"`
}

// HasRole reports whether the identity was granted r.
func (id *Identity) HasRole(r Role) bool { return slices.Contains(id.Roles, r) }

// MaySubscribe reports whether the identity may subscribe to, or list, the given streamer.
func (id *Identity) MaySubscribe(streamerID string) bool {
	return len(id.Streamers) == 0 || slices.Contains(id.Streamers, Wildcard) || slices.Contains(id.Streamers, streamerID)
}

// Pinned reports whether the identity's streamer ids are reserved for it.
func (id *Identity) Pinned() bool {
	return len(id.PublishAs) > 0 && !slices.Contains(id.PublishAs, Wildcard)
}

// MayPublishAs reports whether the identity may register as the given streamer id.
func (id *Identity) MayPublishAs(streamerID string) bool {
	return !id.Pinned() || slices.Contains(id.PublishAs, streamerID)
}

var (
	// ErrNoCredentials means an authenticator found nothing it recognises. The chain moves on.
	ErrNoCredentials = errors.New("no credentials")
	// ErrUnauthorized means credentials were presented and rejected.
	ErrUnauthorized = errors.New("unauthorized")
)

// Authenticator authenticates the request opening a connection. It returns ErrNoCredentials when
// the request carries nothing it handles, and any other error to reject the request.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request, role Role) (*Identity, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(ctx context.Context, r *http.Request, role Role) (*Identity, error)

func (f AuthenticatorFunc) Authenticate(ctx context.Context, r *http.Request, role Role) (*Identity, error) {
	return f(ctx, r, role)
}

// Chain tries authenticators in order. The first identity that grants the requested role wins. If
// any credential was rejected and none accepted, the request is rejected, even where anonymous
// access is allowed: a wrong credential is a mistake worth surfacing, not a reason to fall back.
type Chain struct {
	Authenticators []Authenticator
	// Anonymous lists the roles that may connect without credentials.
	Anonymous []Role
}

func (c *Chain) Authenticate(ctx context.Context, r *http.Request, role Role) (*Identity, error) {
	var rejected error
	for _, a := range c.Authenticators {
		id, err := a.Authenticate(ctx, r, role)
		switch {
		case errors.Is(err, ErrNoCredentials):
			continue
		case err != nil:
			rejected = err
		case id == nil:
			continue
		case !id.HasRole(role):
			rejected = fmt.Errorf("%w: credential does not grant role %s", ErrUnauthorized, role)
		default:
			return id, nil
		}
	}
	if rejected != nil {
		return nil, rejected
	}
	if slices.Contains(c.Anonymous, role) {
		return &Identity{Subject: "anonymous", Method: "anonymous", Roles: []Role{role}}, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrUnauthorized, ErrNoCredentials)
}

// TokenSource says where bearer tokens are read from.
type TokenSource struct {
	// Query is the query parameter carrying a token, for browsers, which cannot set headers on a
	// websocket. Empty disables it. Defaults to "token" via DefaultTokenSource.
	Query string
	// Cookie is the cookie carrying a token, for deployments with a login page on the same origin.
	// Empty disables it. Cookies are only safe because the player listener enforces an Origin check.
	Cookie string
}

// DefaultTokenSource reads the Authorization header and the "token" query parameter, the two places
// the existing Pixel Streaming tooling sends tokens.
var DefaultTokenSource = TokenSource{Query: "token"}

// Tokens returns every bearer token the request presents, header first.
func (s TokenSource) Tokens(r *http.Request) []string {
	var out []string
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		out = append(out, strings.TrimSpace(h[7:]))
	}
	if s.Query != "" {
		out = append(out, r.URL.Query()[s.Query]...)
	}
	if s.Cookie != "" {
		if c, err := r.Cookie(s.Cookie); err == nil {
			out = append(out, c.Value)
		}
	}
	return slices.DeleteFunc(out, func(t string) bool { return t == "" })
}
