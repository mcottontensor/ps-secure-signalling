package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Webhook delegates the decision to an HTTP service. It POSTs a WebhookRequest and accepts the
// connection only on a 200 response whose body is a WebhookResponse with "allow": true and the
// roles granted. Anything else, including a timeout, rejects it.
type Webhook struct {
	URL     string
	Secret  []byte        // if set, the body's HMAC-SHA256 is sent in X-PS-Signature as "sha256=<hex>"
	Timeout time.Duration // default 5s
	// Headers copied from the upgrade request into the webhook request body. Defaults to
	// Authorization and Cookie.
	ForwardHeaders []string
	Source         TokenSource
	Client         *http.Client // optional; must not follow redirects
	// ClientIP gives the address reported as remoteAddr; defaults to the request's RemoteAddr.
	// Set it when behind a trusted proxy so the webhook sees the real client.
	ClientIP func(*http.Request) string
	// MaxConcurrent bounds calls in flight (default 32); further attempts are refused rather than
	// queued, so a flood of connection attempts cannot pile up on the auth service.
	MaxConcurrent int

	once sync.Once
	sem  chan struct{}
}

// WebhookRequest is what the webhook receives.
type WebhookRequest struct {
	Role       Role              `json:"role"`
	RemoteAddr string            `json:"remoteAddr"`
	Path       string            `json:"path"`
	Origin     string            `json:"origin,omitempty"`
	Tokens     []string          `json:"tokens,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	ClientCert string            `json:"clientCert,omitempty"` // verified client certificate subject, if any
	Timestamp  int64             `json:"timestamp"`
}

// WebhookResponse is what the webhook returns. Roles must list what is granted: an allow without
// roles is refused, so a webhook written with players in mind cannot grant admin by omission.
type WebhookResponse struct {
	Allow     bool           `json:"allow"`
	Subject   string         `json:"subject"`
	Roles     []Role         `json:"roles"`
	Streamers []string       `json:"streamers"`
	PublishAs []string       `json:"publishAs"`
	ExpiresAt int64          `json:"expiresAt"` // unix seconds, optional
	Claims    map[string]any `json:"claims"`
}

func (w *Webhook) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (w *Webhook) Authenticate(ctx context.Context, r *http.Request, role Role) (*Identity, error) {
	w.once.Do(func() {
		n := w.MaxConcurrent
		if n <= 0 {
			n = 32
		}
		w.sem = make(chan struct{}, n)
	})
	select {
	case w.sem <- struct{}{}:
		defer func() { <-w.sem }()
	default:
		return nil, fmt.Errorf("%w: webhook busy", ErrUnauthorized)
	}
	headers := w.ForwardHeaders
	if headers == nil {
		headers = []string{"Authorization", "Cookie"}
	}
	body := WebhookRequest{
		Role:       role,
		RemoteAddr: r.RemoteAddr,
		Path:       r.URL.Path,
		Origin:     r.Header.Get("Origin"),
		Tokens:     w.Source.Tokens(r),
		Headers:    map[string]string{},
		Timestamp:  time.Now().Unix(),
	}
	if w.ClientIP != nil {
		body.RemoteAddr = w.ClientIP(r)
	}
	for _, h := range headers {
		if v := r.Header.Get(h); v != "" {
			body.Headers[h] = v
		}
	}
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		body.ClientCert = r.TLS.VerifiedChains[0][0].Subject.String()
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PS-Timestamp", strconv.FormatInt(body.Timestamp, 10))
	if len(w.Secret) > 0 {
		mac := hmac.New(sha256.New, w.Secret)
		mac.Write(data)
		req.Header.Set("X-PS-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := w.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: webhook: %v", ErrUnauthorized, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: webhook: %s", ErrUnauthorized, resp.Status)
	}
	var out WebhookResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: webhook: %v", ErrUnauthorized, err)
	}
	if !out.Allow {
		return nil, fmt.Errorf("%w: webhook denied", ErrUnauthorized)
	}
	id := &Identity{
		Subject:   out.Subject,
		Method:    "webhook",
		Roles:     out.Roles,
		Streamers: out.Streamers,
		PublishAs: out.PublishAs,
		Claims:    out.Claims,
	}
	if len(id.Roles) == 0 {
		return nil, fmt.Errorf("%w: webhook allowed without granting any roles", ErrUnauthorized)
	}
	if out.ExpiresAt > 0 {
		id.ExpiresAt = time.Unix(out.ExpiresAt, 0)
	}
	return id, nil
}
