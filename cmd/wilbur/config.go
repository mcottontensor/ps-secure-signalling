package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/config"
)

// Config is the wilbur configuration file. See wilbur.example.yaml for a commented copy.
type Config struct {
	Log struct {
		Level  string `yaml:"level"`  // debug, info, warn, error
		Format string `yaml:"format"` // text or json
	} `yaml:"log"`

	Listeners struct {
		Player   PlayerListener `yaml:"player"`
		Streamer Listener       `yaml:"streamer"`
		SFU      Listener       `yaml:"sfu"`
	} `yaml:"listeners"`

	Static struct {
		Root     string `yaml:"root"`
		Homepage string `yaml:"homepage"`
		CSP      string `yaml:"csp"`
	} `yaml:"static"`

	RateLimitPerMinute int      `yaml:"rate_limit_per_minute"`
	TrustedProxies     []string `yaml:"trusted_proxies"`
	AllowedOrigins     []string `yaml:"allowed_origins"`

	Signalling struct {
		MaxSubscribers      int            `yaml:"max_subscribers"`
		MaxPlayers          int            `yaml:"max_players"`
		MaxStreamers        int            `yaml:"max_streamers"`
		MaxConnsPerIP       int            `yaml:"max_conns_per_ip"`
		LegacyAutoSubscribe bool           `yaml:"legacy_auto_subscribe"`
		CloseOnExpiry       bool           `yaml:"close_on_expiry"`
		PeerOptions         map[string]any `yaml:"peer_options"`
		PeerOptionsFile     string         `yaml:"peer_options_file"`
	} `yaml:"signalling"`

	TURN struct {
		Secret     string        `yaml:"secret"`
		SecretFile string        `yaml:"secret_file"`
		TTL        time.Duration `yaml:"ttl"`
		// StreamerTTL is the lifetime of streamer and SFU credentials. They are configured once
		// per signalling connection and use them for every player that joins, hence the long default.
		StreamerTTL time.Duration `yaml:"streamer_ttl"`
	} `yaml:"turn"`

	Auth AuthConfig `yaml:"auth"`

	// AllowInsecure starts the server even though the configuration lets anyone connect as a
	// streamer or SFU from outside this machine.
	AllowInsecure bool `yaml:"allow_insecure"`
}

type Listener struct {
	Addr string     `yaml:"addr"` // host:port; empty disables the listener
	TLS  config.TLS `yaml:"tls"`
}

type PlayerListener struct {
	Listener         `yaml:",inline"`
	ServeStatic      bool   `yaml:"serve_static"`
	API              bool   `yaml:"api"`
	HTTPRedirectAddr string `yaml:"http_redirect_addr"` // plain HTTP listener that redirects to the TLS one
}

type AuthConfig struct {
	// Anonymous lists the roles that may connect without credentials.
	Anonymous   []auth.Role `yaml:"anonymous"`
	TokenQuery  string      `yaml:"token_query"`  // query parameter carrying a token (default "token")
	TokenCookie string      `yaml:"token_cookie"` // cookie carrying a token (default none)

	Tokens []struct {
		Token     string      `yaml:"token"`
		TokenFile string      `yaml:"token_file"`
		Subject   string      `yaml:"subject"`
		Roles     []auth.Role `yaml:"roles"`
		Streamers []string    `yaml:"streamers"`
		PublishAs []string    `yaml:"publish_as"`
	} `yaml:"tokens"`

	JWT *struct {
		HMACSecret     string        `yaml:"hmac_secret"`
		HMACSecretFile string        `yaml:"hmac_secret_file"`
		JWKSURL        string        `yaml:"jwks_url"`
		OIDCIssuer     string        `yaml:"oidc_issuer"`
		Issuer         string        `yaml:"issuer"`
		Audience       string        `yaml:"audience"`
		Leeway         time.Duration `yaml:"leeway"`
		Algorithms     []string      `yaml:"algorithms"`
		RolesClaim     string        `yaml:"roles_claim"`
		StreamersClaim string        `yaml:"streamers_claim"`
		PublishClaim   string        `yaml:"publish_claim"`
	} `yaml:"jwt"`

	MTLS *struct {
		Roles            []auth.Role `yaml:"roles"`
		AllowedSubjects  []string    `yaml:"allowed_subjects"`
		PublishAsSubject bool        `yaml:"publish_as_subject"`
		Streamers        []string    `yaml:"streamers"`
	} `yaml:"mtls"`

	Webhook *struct {
		URL            string        `yaml:"url"`
		Secret         string        `yaml:"secret"`
		SecretFile     string        `yaml:"secret_file"`
		Timeout        time.Duration `yaml:"timeout"`
		ForwardHeaders []string      `yaml:"forward_headers"`
	} `yaml:"webhook"`
}

// defaultConfig is what runs with no config file: players on 8080, streamers and SFUs on loopback
// only, which is the only reason they may connect anonymously.
func defaultConfig() *Config {
	c := &Config{}
	c.Log.Level, c.Log.Format = "info", "text"
	c.Listeners.Player.Addr = ":8080"
	c.Listeners.Player.ServeStatic = true
	c.Listeners.Streamer.Addr = "127.0.0.1:8888"
	c.Listeners.SFU.Addr = "127.0.0.1:8889"
	c.Static.Root, c.Static.Homepage = "www", "player.html"
	c.RateLimitPerMinute = 600
	c.Signalling.MaxConnsPerIP = 64
	c.TURN.TTL = 24 * time.Hour
	c.TURN.StreamerTTL = 10 * 365 * 24 * time.Hour
	c.Auth.Anonymous = []auth.Role{auth.RolePlayer, auth.RoleStreamer, auth.RoleSFU}
	c.Auth.TokenQuery = "token"
	return c
}

func loadConfig(path string) (*Config, error) {
	c := defaultConfig()
	if path == "" {
		return c, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // a misspelt security setting must not be silently ignored
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// validate refuses configurations that are unsafe by accident, and returns warnings for ones that
// may be unsafe on purpose.
func (c *Config) validate() (warnings []string, err error) {
	var errs []error
	if slices.Contains(c.Auth.Anonymous, auth.RoleAdmin) {
		errs = append(errs, errors.New("auth.anonymous must not include admin"))
	}
	if c.Listeners.Player.Addr == "" {
		errs = append(errs, errors.New("listeners.player.addr is required"))
	}
	for _, l := range []struct {
		role auth.Role
		addr string
	}{{auth.RoleStreamer, c.Listeners.Streamer.Addr}, {auth.RoleSFU, c.Listeners.SFU.Addr}} {
		if l.addr == "" || !slices.Contains(c.Auth.Anonymous, l.role) || isLoopback(l.addr) {
			continue
		}
		msg := fmt.Sprintf("anyone who can reach %s can connect as a %s: auth.anonymous includes %s and the listener is not loopback", l.addr, l.role, l.role)
		if !c.AllowInsecure {
			errs = append(errs, errors.New(msg+" (set allow_insecure: true if that is intended)"))
		} else {
			warnings = append(warnings, msg)
		}
	}
	if c.Listeners.Player.API && !c.hasCredentialAuth() {
		errs = append(errs, errors.New("listeners.player.api needs an authenticator granting the admin role"))
	}
	if !c.Listeners.Player.TLS.Enabled() && !isLoopback(c.Listeners.Player.Addr) {
		warnings = append(warnings, "the player listener does not use TLS; put it behind a TLS-terminating proxy")
	}
	for _, t := range c.Auth.Tokens {
		if t.Token != "" {
			warnings = append(warnings, "auth.tokens has an inline token; prefer token_file")
			break
		}
	}
	if j := c.Auth.JWT; j != nil && (j.OIDCIssuer != "" || j.JWKSURL != "") && j.Audience == "" {
		warnings = append(warnings, "auth.jwt has no audience: tokens your identity provider issues for other applications are accepted if they carry ps_roles")
	}
	for _, p := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			errs = append(errs, fmt.Errorf("trusted_proxies: %w", err))
		}
	}
	if c.Signalling.PeerOptions != nil && c.Signalling.PeerOptionsFile != "" {
		errs = append(errs, errors.New("signalling: give peer_options or peer_options_file, not both"))
	}
	return warnings, errors.Join(errs...)
}

func (c *Config) hasCredentialAuth() bool {
	return len(c.Auth.Tokens) > 0 || c.Auth.JWT != nil || c.Auth.MTLS != nil || c.Auth.Webhook != nil
}

// reservedStreamerIDs are the ids static tokens reserve, protected from the start even while
// their owners are offline.
func (c *Config) reservedStreamerIDs() []string {
	var ids []string
	for _, t := range c.Auth.Tokens {
		for _, id := range t.PublishAs {
			if id != auth.Wildcard {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func (c *Config) trustedProxies() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range c.TrustedProxies {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

// peerOptions returns the peerConnectionOptions as JSON.
func (c *Config) peerOptions() (json.RawMessage, error) {
	if c.Signalling.PeerOptionsFile != "" {
		b, err := os.ReadFile(c.Signalling.PeerOptionsFile)
		if err != nil {
			return nil, err
		}
		if !json.Valid(b) {
			return nil, fmt.Errorf("%s is not valid JSON", c.Signalling.PeerOptionsFile)
		}
		return b, nil
	}
	if c.Signalling.PeerOptions == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.Marshal(c.Signalling.PeerOptions)
}

// isLoopback reports whether a listen address only accepts connections from this machine.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
