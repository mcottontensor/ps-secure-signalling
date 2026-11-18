package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// JWTConfig configures JWT bearer token authentication. Supply an HMAC secret, a JWKS URL, an OIDC
// issuer (whose JWKS is found by discovery), or a mix.
type JWTConfig struct {
	HMACSecret []byte
	JWKSURL    string
	OIDCIssuer string

	Issuer   string        // required "iss", if set (defaults to OIDCIssuer)
	Audience string        // required "aud", if set
	Leeway   time.Duration // clock skew allowance

	// Algorithms allowed. Defaults to HS256 for an HMAC secret, and RS256, PS256, ES256 and EdDSA
	// for public keys. "none" is never accepted.
	Algorithms []string

	// Claim names. Defaults: "ps_roles", "ps_streamers", "ps_publish". Each may be a string or an
	// array of strings in the token.
	RolesClaim, StreamersClaim, PublishClaim string
}

// JWT authenticates signed bearer tokens. Tokens must carry an expiry.
type JWT struct {
	Source TokenSource
	cfg    JWTConfig
	parser *jwt.Parser
	jwks   keyfunc.Keyfunc
}

// NewJWT builds the authenticator. With a JWKS or OIDC issuer it fetches keys now, and refreshes
// them in the background until ctx ends.
func NewJWT(ctx context.Context, src TokenSource, cfg JWTConfig) (*JWT, error) {
	if len(cfg.HMACSecret) == 0 && cfg.JWKSURL == "" && cfg.OIDCIssuer == "" {
		return nil, errors.New("auth: jwt needs an hmac secret, jwks url or oidc issuer")
	}
	if len(cfg.HMACSecret) > 0 && len(cfg.HMACSecret) < 32 {
		return nil, errors.New("auth: jwt hmac secret must be at least 32 bytes")
	}
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = "ps_roles"
	}
	if cfg.StreamersClaim == "" {
		cfg.StreamersClaim = "ps_streamers"
	}
	if cfg.PublishClaim == "" {
		cfg.PublishClaim = "ps_publish"
	}
	if cfg.Issuer == "" {
		cfg.Issuer = cfg.OIDCIssuer
	}
	if cfg.OIDCIssuer != "" && cfg.JWKSURL == "" {
		u, err := discoverJWKS(ctx, cfg.OIDCIssuer)
		if err != nil {
			return nil, err
		}
		cfg.JWKSURL = u
	}
	if len(cfg.Algorithms) == 0 {
		if len(cfg.HMACSecret) > 0 {
			cfg.Algorithms = append(cfg.Algorithms, "HS256")
		}
		if cfg.JWKSURL != "" {
			cfg.Algorithms = append(cfg.Algorithms, "RS256", "PS256", "ES256", "EdDSA")
		}
	}
	j := &JWT{Source: src, cfg: cfg}
	if cfg.JWKSURL != "" {
		k, err := keyfunc.NewDefaultCtx(ctx, []string{cfg.JWKSURL})
		if err != nil {
			return nil, fmt.Errorf("auth: jwks: %w", err)
		}
		j.jwks = k
	}
	opts := []jwt.ParserOption{jwt.WithValidMethods(cfg.Algorithms), jwt.WithExpirationRequired(), jwt.WithLeeway(cfg.Leeway)}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.Audience))
	}
	j.parser = jwt.NewParser(opts...)
	return j, nil
}

func (j *JWT) keyFor(t *jwt.Token) (any, error) {
	if _, ok := t.Method.(*jwt.SigningMethodHMAC); ok {
		if len(j.cfg.HMACSecret) == 0 {
			return nil, errors.New("hmac tokens not accepted")
		}
		return j.cfg.HMACSecret, nil
	}
	if j.jwks == nil {
		return nil, errors.New("public key tokens not accepted")
	}
	return j.jwks.Keyfunc(t)
}

func (j *JWT) Authenticate(_ context.Context, r *http.Request, _ Role) (*Identity, error) {
	var rejected error
	for _, tok := range j.Source.Tokens(r) {
		if strings.Count(tok, ".") != 2 {
			continue // not a JWT; perhaps a static token
		}
		claims := jwt.MapClaims{}
		if _, err := j.parser.ParseWithClaims(tok, claims, j.keyFor); err != nil {
			rejected = fmt.Errorf("%w: jwt: %v", ErrUnauthorized, err)
			continue
		}
		id := &Identity{Method: "jwt", Claims: claims}
		id.Subject, _ = claims.GetSubject()
		if exp, _ := claims.GetExpirationTime(); exp != nil {
			id.ExpiresAt = exp.Time
		}
		for _, r := range stringList(claims[j.cfg.RolesClaim]) {
			id.Roles = append(id.Roles, Role(r))
		}
		id.Streamers = stringList(claims[j.cfg.StreamersClaim])
		id.PublishAs = stringList(claims[j.cfg.PublishClaim])
		return id, nil
	}
	if rejected != nil {
		return nil, rejected
	}
	return nil, ErrNoCredentials
}

// stringList reads a claim that may be a single string or an array of strings.
func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		return strings.Fields(t)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func discoverJWKS(ctx context.Context, issuer string) (string, error) {
	u := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth: oidc discovery: %s", resp.Status)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("auth: oidc discovery: %w", err)
	}
	if doc.JWKSURI == "" || strings.TrimSuffix(doc.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return "", errors.New("auth: oidc discovery: document does not match issuer")
	}
	return doc.JWKSURI, nil
}
