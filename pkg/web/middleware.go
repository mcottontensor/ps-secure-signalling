// Package web holds the HTTP pieces around the signalling endpoints: static file serving for the
// player page, the REST API, and the middleware they share.
package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// DefaultCSP suits the stock Pixel Streaming frontend: same-origin scripts, styles injected at
// runtime (hence 'unsafe-inline' for styles only), the Google Fonts the stock page links, media
// from blobs, and websocket connections to anywhere, since the signalling URL is configurable.
const DefaultCSP = "default-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; " +
	"img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self' ws: wss:; " +
	"frame-ancestors 'self'; base-uri 'self'; form-action 'self'; object-src 'none'"

// Headers adds security headers to every response. hsts should only be set when the site is
// served over TLS.
func Headers(next http.Handler, csp string, hsts bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if csp != "" {
			h.Set("Content-Security-Policy", csp)
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), geolocation=(), payment=(), usb=()")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=15552000")
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIPFunc returns a function giving the client address of a request. X-Forwarded-For is only
// believed when the direct peer is one of the trusted proxies, and then only as far back as the
// first untrusted hop, so a client cannot choose its own address.
func ClientIPFunc(trusted []netip.Prefix) func(*http.Request) string {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return false
	}
	return func(r *http.Request) string {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !isTrusted(addr) {
			return host
		}
		hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				break
			}
			if !isTrusted(hop) {
				return hop.String()
			}
			host = hop.String()
		}
		return host
	}
}

// RateLimit limits each client address to perMinute requests, with a burst of a tenth of that.
func RateLimit(next http.Handler, perMinute int, clientIP func(*http.Request) string) http.Handler {
	if perMinute <= 0 {
		return next
	}
	type entry struct {
		lim  *rate.Limiter
		seen time.Time
	}
	var mu sync.Mutex
	clients := map[string]*entry{}
	lastSweep := time.Now()
	burst := max(perMinute/10, 1)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		now := time.Now()
		mu.Lock()
		if now.Sub(lastSweep) > time.Minute {
			for k, e := range clients {
				if now.Sub(e.seen) > 5*time.Minute {
					delete(clients, k)
				}
			}
			lastSweep = now
		}
		e := clients[ip]
		if e == nil {
			e = &entry{lim: rate.NewLimiter(rate.Limit(float64(perMinute)/60), burst)}
			clients[ip] = e
		}
		e.seen = now
		ok := e.lim.Allow()
		mu.Unlock()
		if !ok {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RedirectHTTPS redirects every request to the same host on httpsPort.
func RedirectHTTPS(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if httpsPort != "443" {
			host = net.JoinHostPort(host, httpsPort)
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}
