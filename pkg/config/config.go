// Package config holds the pieces of configuration shared by the binaries: secrets read from
// files, and TLS settings for listeners and clients.
package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
)

// Secret reads a secret given either inline or as a file. Files are preferred: an inline secret
// ends up in the config file, process listings or shell history. A trailing newline is removed.
func Secret(value, file string) ([]byte, error) {
	if value != "" && file != "" {
		return nil, errors.New("config: give a secret inline or as a file, not both")
	}
	if file == "" {
		return []byte(value), nil
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(file); err == nil && st.Mode().Perm()&0o077 != 0 {
			slog.Warn("secret file is readable by other users", "file", file, "mode", st.Mode().Perm().String())
		}
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	b = bytes.TrimRight(b, "\r\n")
	if len(b) == 0 {
		return nil, fmt.Errorf("config: secret file %s is empty", file)
	}
	return b, nil
}

// TLS configures TLS for a listener or a client.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ClientCAFile, on a listener, enables client certificates verified against this CA bundle.
	// On a client, it is the CA bundle used to verify the server instead of the system roots.
	ClientCAFile string `yaml:"client_ca_file"`
	// RequireClientCert refuses TLS handshakes without a valid client certificate. Without it,
	// certificates are verified when presented and other authenticators can still be used.
	RequireClientCert bool `yaml:"require_client_cert"`
}

// Enabled reports whether a certificate is configured.
func (t TLS) Enabled() bool { return t.CertFile != "" }

// Server builds a listener TLS configuration, or nil if TLS is not configured.
func (t TLS) Server() (*tls.Config, error) {
	if !t.Enabled() {
		if t.ClientCAFile != "" {
			return nil, errors.New("config: client_ca_file needs cert_file and key_file")
		}
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if t.ClientCAFile != "" {
		pool, err := loadPool(t.ClientCAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
		if t.RequireClientCert {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}
	return cfg, nil
}

// ClientTLS configures TLS for an outgoing connection.
type ClientTLS struct {
	CAFile   string `yaml:"ca_file"`   // verify the server against this bundle instead of system roots
	CertFile string `yaml:"cert_file"` // client certificate, for mTLS
	KeyFile  string `yaml:"key_file"`
	// InsecureSkipVerify disables server verification. For local testing only.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// Client builds the client TLS configuration.
func (c ClientTLS) Client() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureSkipVerify}
	if c.CAFile != "" {
		pool, err := loadPool(c.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

func loadPool(file string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("config: no certificates in %s", file)
	}
	return pool, nil
}
