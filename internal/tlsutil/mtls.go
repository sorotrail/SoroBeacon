// Package tlsutil provides TLS 1.2+/1.3 server and client configuration with
// mutual TLS (mTLS) verification, startup certificate/key/CA validation,
// live certificate hot-reloading on SIGHUP without dropping active connections,
// and leaf certificate expiration tracking.
package tlsutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ExpiryWarningWindow is the threshold before leaf certificate expiration at
// which startup and reload emit a warning log.
const ExpiryWarningWindow = 14 * 24 * time.Hour

// ClientAuthMode names the client certificate verification policy for a TLS
// server.
type ClientAuthMode string

const (
	ClientAuthNone    ClientAuthMode = "none"
	ClientAuthRequest ClientAuthMode = "request"
	ClientAuthRequire ClientAuthMode = "require"
)

// ParseClientAuthMode normalises and validates a TLS_CLIENT_AUTH string.
// When empty, it defaults to "require" if hasClientCA is true, or "none"
// otherwise.
func ParseClientAuthMode(raw string, hasClientCA bool) (ClientAuthMode, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		if hasClientCA {
			return ClientAuthRequire, nil
		}
		return ClientAuthNone, nil
	}
	switch ClientAuthMode(v) {
	case ClientAuthNone, ClientAuthRequest, ClientAuthRequire:
		return ClientAuthMode(v), nil
	default:
		return "", fmt.Errorf("must be one of: none, request, require (got %q)", raw)
	}
}

func (m ClientAuthMode) toStd() tls.ClientAuthType {
	switch m {
	case ClientAuthRequest:
		return tls.VerifyClientCertIfGiven
	case ClientAuthRequire:
		return tls.RequireAndVerifyClientCert
	default:
		return tls.NoClientCert
	}
}

// PeerConfig holds the file paths and verification settings for a TLS server
// or client endpoint.
type PeerConfig struct {
	Component  string
	CertFile   string
	KeyFile    string
	CAFile     string
	ClientAuth ClientAuthMode
	ServerName string
	Now        func() time.Time
	Logger     *slog.Logger
}

// LoadedBundle is a validated snapshot of a certificate/key pair and CA pool.
type LoadedBundle struct {
	Cert       *tls.Certificate
	Leaf       *x509.Certificate
	CAPool     *x509.CertPool
	ClientAuth tls.ClientAuthType
	ServerName string
}

// ValidateFiles loads and validates the configured certificate, private key,
// and CA bundle at the given point in time.
func ValidateFiles(cfg PeerConfig, now time.Time) (*LoadedBundle, error) {
	if now.IsZero() {
		now = time.Now()
	}
	certFile := strings.TrimSpace(cfg.CertFile)
	keyFile := strings.TrimSpace(cfg.KeyFile)
	caFile := strings.TrimSpace(cfg.CAFile)

	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("both certificate file and private key file must be set together")
	}

	out := &LoadedBundle{
		ClientAuth: cfg.ClientAuth.toStd(),
		ServerName: strings.TrimSpace(cfg.ServerName),
	}

	if certFile != "" && keyFile != "" {
		certPEM, err := os.ReadFile(certFile) // #nosec G304 -- certFile comes from operator-configured TLS settings
		if err != nil {
			return nil, fmt.Errorf("read certificate file %q: %w", certFile, err)
		}
		keyPEM, err := os.ReadFile(keyFile) // #nosec G304 -- keyFile comes from operator-configured TLS settings
		if err != nil {
			return nil, fmt.Errorf("read private key file %q: %w", keyFile, err)
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load certificate/key pair (%q, %q): %w", certFile, keyFile, err)
		}
		if len(pair.Certificate) == 0 {
			return nil, fmt.Errorf("certificate file %q contains no certificate", certFile)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("parse leaf certificate in %q: %w", certFile, err)
		}
		if now.After(leaf.NotAfter) {
			return nil, fmt.Errorf("certificate in %q expired at %s", certFile, leaf.NotAfter.UTC().Format(time.RFC3339))
		}
		if now.Before(leaf.NotBefore) {
			return nil, fmt.Errorf("certificate in %q is not valid until %s", certFile, leaf.NotBefore.UTC().Format(time.RFC3339))
		}
		pair.Leaf = leaf
		out.Cert = &pair
		out.Leaf = leaf
	}

	if caFile != "" {
		caPEM, err := os.ReadFile(caFile) // #nosec G304 -- caFile comes from operator-configured TLS settings
		if err != nil {
			return nil, fmt.Errorf("read CA bundle %q: %w", caFile, err)
		}
		pool, err := parseCABundle(caPEM, caFile, now)
		if err != nil {
			return nil, err
		}
		out.CAPool = pool
	}

	if out.ClientAuth == tls.RequireAndVerifyClientCert && out.CAPool == nil {
		return nil, errors.New("client certificate verification requires a CA bundle file")
	}

	return out, nil
}

func parseCABundle(pemBytes []byte, path string, now time.Time) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	rest := pemBytes
	count := 0
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA certificate in %q: %w", path, err)
		}
		if now.After(cert.NotAfter) {
			return nil, fmt.Errorf("CA certificate %q in %q expired at %s", cert.Subject.String(), path, cert.NotAfter.UTC().Format(time.RFC3339))
		}
		pool.AddCert(cert)
		count++
	}
	if count == 0 {
		return nil, fmt.Errorf("CA bundle %q contains no valid PEM certificates", path)
	}
	return pool, nil
}

// Reloader manages a TLS certificate/key pair and CA bundle, supporting
// atomic in-memory hot-reloading on SIGHUP without dropping active connections.
type Reloader struct {
	cfg PeerConfig
	now func() time.Time
	log *slog.Logger

	mu     sync.RWMutex
	bundle *LoadedBundle
}

// NewReloader validates the initial TLS files from disk and returns a Reloader
// ready to build server or client *tls.Config instances.
func NewReloader(cfg PeerConfig) (*Reloader, error) {
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	r := &Reloader{
		cfg: cfg,
		now: nowFn,
		log: log,
	}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads the certificate, key, and CA bundle from disk. If validation
// fails, the previously loaded bundle stays in effect and an error is returned.
func (r *Reloader) Reload() error {
	now := r.now()
	bundle, err := ValidateFiles(r.cfg, now)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.bundle = bundle
	r.mu.Unlock()

	if bundle.Leaf != nil {
		rem := bundle.Leaf.NotAfter.Sub(now)
		if rem <= ExpiryWarningWindow {
			r.log.Warn("tls certificate is close to expiration",
				"component", r.cfg.Component,
				"cert_file", r.cfg.CertFile,
				"not_after", bundle.Leaf.NotAfter.UTC().Format(time.RFC3339),
				"days_remaining", rem.Hours()/24,
			)
		}
	}
	return nil
}

func (r *Reloader) currentBundle() *LoadedBundle {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bundle
}

// HasCertificate reports whether a leaf certificate/key pair is loaded.
func (r *Reloader) HasCertificate() bool {
	if r == nil {
		return false
	}
	b := r.currentBundle()
	return b != nil && b.Cert != nil
}

// LeafNotAfterUnix returns the loaded leaf certificate's NotAfter timestamp in
// Unix seconds, or 0 when no leaf certificate is configured.
func (r *Reloader) LeafNotAfterUnix() float64 {
	if r == nil {
		return 0
	}
	b := r.currentBundle()
	if b == nil || b.Leaf == nil {
		return 0
	}
	return float64(b.Leaf.NotAfter.Unix())
}

// LeafDaysRemaining returns the number of days until the loaded leaf
// certificate expires relative to now, or 0 when no leaf certificate is loaded.
func (r *Reloader) LeafDaysRemaining(now time.Time) float64 {
	if r == nil {
		return 0
	}
	b := r.currentBundle()
	if b == nil || b.Leaf == nil {
		return 0
	}
	if now.IsZero() {
		now = r.now()
	}
	return b.Leaf.NotAfter.Sub(now).Hours() / 24
}

// ServerTLSConfig returns a *tls.Config for an HTTP or gRPC server. It uses
// GetConfigForClient so every new TLS handshake picks up the latest reloaded
// server certificate, client CA pool, and client auth policy without restarting
// the listener.
func (r *Reloader) ServerTLSConfig() (*tls.Config, error) {
	if r == nil || !r.HasCertificate() {
		return nil, errors.New("server TLS requires a certificate and private key")
	}
	buildSnapshot := func() *tls.Config {
		b := r.currentBundle()
		cfg := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{*b.Cert},
			ClientAuth:   b.ClientAuth,
			ClientCAs:    b.CAPool,
			NextProtos:   []string{"h2", "http/1.1"},
		}
		return cfg
	}

	base := buildSnapshot()
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return buildSnapshot(), nil
	}
	return base, nil
}

// ClientTLSConfig returns a snapshot *tls.Config for an outbound TLS/mTLS
// client connection. GetClientCertificate dynamically reads the latest
// reloaded client certificate on every handshake.
func (r *Reloader) ClientTLSConfig() *tls.Config {
	if r == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	b := r.currentBundle()
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    b.CAPool,
		ServerName: b.ServerName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			curr := r.currentBundle()
			if curr != nil && curr.Cert != nil {
				return curr.Cert, nil
			}
			return &tls.Certificate{}, nil
		},
	}
	return cfg
}

// NewHTTPTransport returns an *http.Transport cloned from http.DefaultTransport
// whose TLS handshakes dynamically use the Reloader's current client
// certificate and root CA bundle on every new connection.
func (r *Reloader) NewHTTPTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	var tr *http.Transport
	if ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr.TLSClientConfig = r.ClientTLSConfig()
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		rawConn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		tlsCfg := r.ClientTLSConfig()
		if tlsCfg.ServerName == "" {
			host, _, splitErr := net.SplitHostPort(addr)
			if splitErr == nil {
				tlsCfg.ServerName = host
			} else {
				tlsCfg.ServerName = addr
			}
		}
		conn := tls.Client(rawConn, tlsCfg)
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
		return conn, nil
	}
	return tr
}
