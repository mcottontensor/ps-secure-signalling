// Command wilbur is a Pixel Streaming signalling and web server: a security-focused replacement for
// the reference SignallingWebServer, speaking the same protocol.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/config"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/signalling"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/turn"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("wilbur failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to the YAML configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(log)

	warnings, err := cfg.validate()
	for _, w := range warnings {
		log.Warn("configuration: " + w)
	}
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	peerOptions, err := cfg.peerOptions()
	if err != nil {
		return err
	}
	clientIP := web.ClientIPFunc(cfg.trustedProxies())

	connAuth, err := buildAuth(ctx, cfg, auth.TokenSource{Query: cfg.Auth.TokenQuery, Cookie: cfg.Auth.TokenCookie}, clientIP)
	if err != nil {
		return err
	}
	hooks, err := buildHooks(cfg, peerOptions)
	if err != nil {
		return err
	}

	srv := signalling.New(signalling.Config{
		PeerOptions:         peerOptions,
		MaxSubscribers:      cfg.Signalling.MaxSubscribers,
		MaxPlayers:          cfg.Signalling.MaxPlayers,
		MaxStreamers:        cfg.Signalling.MaxStreamers,
		MaxConnsPerIP:       cfg.Signalling.MaxConnsPerIP,
		AllowedOrigins:      cfg.AllowedOrigins,
		LegacyAutoSubscribe: cfg.Signalling.LegacyAutoSubscribe,
		CloseOnExpiry:       cfg.Signalling.CloseOnExpiry,
		ReservedStreamerIDs: cfg.reservedStreamerIDs(),
		ClientIP:            clientIP,
		Logger:              log,
	}, connAuth, hooks)

	playerHandler, err := buildPlayerHandler(ctx, cfg, srv, clientIP, log)
	if err != nil {
		return err
	}

	var servers []*http.Server
	start := func(name string, l Listener, h http.Handler) error {
		if l.Addr == "" {
			return nil
		}
		// Every request, websocket upgrades included, counts against the per-address rate.
		h = web.RateLimit(h, cfg.RateLimitPerMinute, clientIP)
		tlsCfg, err := l.TLS.Server()
		if err != nil {
			return fmt.Errorf("%s listener: %w", name, err)
		}
		hs, err := listen(name, l.Addr, tlsCfg, h, log)
		if err != nil {
			return err
		}
		servers = append(servers, hs)
		return nil
	}
	if err := start("player", cfg.Listeners.Player.Listener, playerHandler); err != nil {
		return err
	}
	if err := start("streamer", cfg.Listeners.Streamer, srv.Handler(auth.RoleStreamer)); err != nil {
		return err
	}
	if err := start("sfu", cfg.Listeners.SFU, srv.Handler(auth.RoleSFU)); err != nil {
		return err
	}
	if addr := cfg.Listeners.Player.HTTPRedirectAddr; addr != "" {
		_, port, _ := net.SplitHostPort(cfg.Listeners.Player.Addr)
		if err := start("http-redirect", Listener{Addr: addr}, web.RedirectHTTPS(port)); err != nil {
			return err
		}
	}

	<-ctx.Done()
	log.Info("shutting down")
	srv.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, hs := range servers {
		_ = hs.Shutdown(shutdownCtx)
	}
	return nil
}

// listen starts one HTTP server. Timeouts bound how long a client may hold a connection without
// completing a request; websockets are hijacked and governed by wsconn instead.
func listen(name, addr string, tlsCfg *tls.Config, h http.Handler, log *slog.Logger) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%s listener: %w", name, err)
	}
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	hs := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listener stopped", "listener", name, "err", err)
		}
	}()
	log.Info("listening", "listener", name, "addr", ln.Addr().String(), "tls", tlsCfg != nil)
	return hs, nil
}

// buildPlayerHandler serves player websockets, the static site and the REST API from one port, as
// the frontend expects to find the signalling server where it loaded the page from.
func buildPlayerHandler(ctx context.Context, cfg *Config, srv *signalling.Server, clientIP func(*http.Request) string, log *slog.Logger) (http.Handler, error) {
	ws := srv.Handler(auth.RolePlayer)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	if cfg.Listeners.Player.API {
		// The API reads credentials from the Authorization header only, never the URL.
		apiAuth, err := buildAuth(ctx, cfg, auth.TokenSource{}, clientIP)
		if err != nil {
			return nil, err
		}
		mux.Handle("/api/", web.API(srv, apiAuth, version, log))
	}
	if cfg.Listeners.Player.ServeStatic {
		static, err := web.Static(cfg.Static.Root, cfg.Static.Homepage)
		if err != nil {
			return nil, err
		}
		mux.Handle("/", static)
	} else {
		mux.Handle("/", http.NotFoundHandler())
	}
	csp := cfg.Static.CSP
	if csp == "" {
		csp = web.DefaultCSP
	}
	site := web.Headers(mux, csp, cfg.Listeners.Player.TLS.Enabled())

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			ws.ServeHTTP(w, r)
			return
		}
		site.ServeHTTP(w, r)
	}), nil
}

// buildAuth assembles the authenticator chain from the configuration.
func buildAuth(ctx context.Context, cfg *Config, src auth.TokenSource, clientIP func(*http.Request) string) (auth.Authenticator, error) {
	a := cfg.Auth
	chain := &auth.Chain{Anonymous: a.Anonymous}

	if len(a.Tokens) > 0 {
		var tokens []auth.StaticToken
		for _, t := range a.Tokens {
			secret, err := config.Secret(t.Token, t.TokenFile)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, auth.StaticToken{Token: string(secret), Subject: t.Subject, Roles: t.Roles, Streamers: t.Streamers, PublishAs: t.PublishAs})
		}
		st, err := auth.NewStaticTokens(src, tokens)
		if err != nil {
			return nil, err
		}
		chain.Authenticators = append(chain.Authenticators, st)
	}
	if a.MTLS != nil {
		chain.Authenticators = append(chain.Authenticators, &auth.MTLS{
			Roles: a.MTLS.Roles, AllowedSubjects: a.MTLS.AllowedSubjects,
			PublishAsSubject: a.MTLS.PublishAsSubject, Streamers: a.MTLS.Streamers,
		})
	}
	if j := a.JWT; j != nil {
		var secret []byte
		if j.HMACSecret != "" || j.HMACSecretFile != "" {
			var err error
			if secret, err = config.Secret(j.HMACSecret, j.HMACSecretFile); err != nil {
				return nil, err
			}
		}
		ja, err := auth.NewJWT(ctx, src, auth.JWTConfig{
			HMACSecret: secret, JWKSURL: j.JWKSURL, OIDCIssuer: j.OIDCIssuer, Issuer: j.Issuer,
			Audience: j.Audience, Leeway: j.Leeway, Algorithms: j.Algorithms,
			RolesClaim: j.RolesClaim, StreamersClaim: j.StreamersClaim, PublishClaim: j.PublishClaim,
		})
		if err != nil {
			return nil, err
		}
		chain.Authenticators = append(chain.Authenticators, ja)
	}
	if w := a.Webhook; w != nil {
		var secret []byte
		if w.Secret != "" || w.SecretFile != "" {
			var err error
			if secret, err = config.Secret(w.Secret, w.SecretFile); err != nil {
				return nil, err
			}
		}
		chain.Authenticators = append(chain.Authenticators, &auth.Webhook{
			URL: w.URL, Secret: secret, Timeout: w.Timeout, ForwardHeaders: w.ForwardHeaders, Source: src, ClientIP: clientIP,
		})
	}
	return chain, nil
}

// buildHooks wires the configured behaviour that is expressed as hooks: TURN credential minting.
func buildHooks(cfg *Config, peerOptions json.RawMessage) (signalling.Hooks, error) {
	var hooks signalling.Hooks
	if cfg.TURN.Secret == "" && cfg.TURN.SecretFile == "" {
		return hooks, nil
	}
	secret, err := config.Secret(cfg.TURN.Secret, cfg.TURN.SecretFile)
	if err != nil {
		return hooks, err
	}
	hooks.PeerOptions = func(info signalling.PeerInfo) json.RawMessage {
		// Players reconnect often and get short-lived credentials. Streamers and SFUs are
		// configured once when they connect and keep their credentials for as long as they run.
		ttl, user := cfg.TURN.TTL, info.PlayerID
		if info.Role != auth.RolePlayer {
			ttl, user = cfg.TURN.StreamerTTL, info.StreamerID
		}
		opts, err := turn.Apply(peerOptions, secret, ttl, user)
		if err != nil {
			slog.Error("minting TURN credentials failed; sending static peer options", "err", err)
			return nil
		}
		return opts
	}
	return hooks, nil
}

func newLogger(level, format string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
