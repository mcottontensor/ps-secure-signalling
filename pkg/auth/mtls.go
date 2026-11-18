package auth

import (
	"context"
	"crypto/x509"
	"net/http"
	"slices"
)

// MTLS authenticates TLS client certificates. The listener must be configured to request and
// verify them (see config.TLS.ClientCAFile); only verified chains are considered.
type MTLS struct {
	// Roles granted to a verified certificate.
	Roles []Role
	// AllowedSubjects restricts which certificates are accepted, by common name or DNS/URI SAN.
	// Empty accepts any certificate the client CA verified.
	AllowedSubjects []string
	// PublishAsSubject reserves the certificate's subject as the only streamer id it may register
	// as, so a CA can issue one certificate per streamer.
	PublishAsSubject bool
	// Streamers restricts what the certificate may subscribe to. Empty means no restriction.
	Streamers []string
}

func (m *MTLS) Authenticate(_ context.Context, r *http.Request, _ Role) (*Identity, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return nil, ErrNoCredentials
	}
	cert := r.TLS.VerifiedChains[0][0]
	names := certNames(cert)
	if len(m.AllowedSubjects) > 0 && !slices.ContainsFunc(names, func(n string) bool { return slices.Contains(m.AllowedSubjects, n) }) {
		return nil, ErrUnauthorized
	}
	subject := ""
	if len(names) > 0 {
		subject = names[0]
	}
	id := &Identity{
		Subject:   subject,
		Method:    "mtls",
		Roles:     m.Roles,
		Streamers: m.Streamers,
		ExpiresAt: cert.NotAfter,
		Claims:    map[string]any{"serial": cert.SerialNumber.String(), "names": names},
	}
	if m.PublishAsSubject && subject != "" {
		id.PublishAs = []string{subject}
	}
	return id, nil
}

// certNames lists the certificate's common name, then its DNS and URI SANs.
func certNames(c *x509.Certificate) []string {
	var names []string
	if c.Subject.CommonName != "" {
		names = append(names, c.Subject.CommonName)
	}
	names = append(names, c.DNSNames...)
	for _, u := range c.URIs {
		names = append(names, u.String())
	}
	return names
}
