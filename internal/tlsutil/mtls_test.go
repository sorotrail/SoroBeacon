package tlsutil

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testPKI struct {
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	caPEM     []byte
	caFile    string
	notBefore time.Time
	notAfter  time.Time
}

func newTestCA(t *testing.T, dir, name string, notBefore, notAfter time.Time) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caFile := filepath.Join(dir, name+"-ca.pem")
	require.NoError(t, os.WriteFile(caFile, caPEM, 0o600))

	return &testPKI{
		caCert:    cert,
		caKey:     key,
		caPEM:     caPEM,
		caFile:    caFile,
		notBefore: notBefore,
		notAfter:  notAfter,
	}
}

func (p *testPKI) issueLeaf(t *testing.T, dir, name, cn string, spiffeURI string, notBefore, notAfter time.Time, isServer bool) (certFile, keyFile string, certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if isServer {
		eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  eku,
		DNSNames:     []string{"localhost", cn},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	if spiffeURI != "" {
		u, uErr := url.Parse(spiffeURI)
		require.NoError(t, uErr)
		tmpl.URIs = []*url.URL{u}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certFile = filepath.Join(dir, name+"-cert.pem")
	keyFile = filepath.Join(dir, name+"-key.pem")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))
	return certFile, keyFile, certPEM, keyPEM
}

func TestValidateFiles_ErrorsOnInvalidOrExpiredCerts(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	ca := newTestCA(t, dir, "root", now.Add(-24*time.Hour), now.Add(365*24*time.Hour))

	validCert, validKey, _, _ := ca.issueLeaf(t, dir, "valid", "server", "", now.Add(-time.Hour), now.Add(30*24*time.Hour), true)
	_, otherKey, _, _ := ca.issueLeaf(t, dir, "other", "other", "", now.Add(-time.Hour), now.Add(30*24*time.Hour), true)
	expiredCert, expiredKey, _, _ := ca.issueLeaf(t, dir, "expired", "expired", "", now.Add(-48*time.Hour), now.Add(-time.Hour), true)

	t.Run("missing key file when cert file is set", func(t *testing.T) {
		_, err := ValidateFiles(PeerConfig{CertFile: validCert}, now)
		require.ErrorContains(t, err, "both certificate file and private key file must be set together")
	})

	t.Run("mismatched cert and private key", func(t *testing.T) {
		_, err := ValidateFiles(PeerConfig{CertFile: validCert, KeyFile: otherKey}, now)
		require.ErrorContains(t, err, "load certificate/key pair")
	})

	t.Run("expired leaf certificate", func(t *testing.T) {
		_, err := ValidateFiles(PeerConfig{CertFile: expiredCert, KeyFile: expiredKey}, now)
		require.ErrorContains(t, err, "expired at")
	})

	t.Run("require client auth without CA file fails", func(t *testing.T) {
		_, err := ValidateFiles(PeerConfig{
			CertFile:   validCert,
			KeyFile:    validKey,
			ClientAuth: ClientAuthRequire,
		}, now)
		require.ErrorContains(t, err, "requires a CA bundle file")
	})
}

func TestMTLSHandshake_ValidAndRejectedCases(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	ca := newTestCA(t, dir, "trusted", now.Add(-time.Hour), now.Add(30*24*time.Hour))
	rogueCA := newTestCA(t, dir, "rogue", now.Add(-time.Hour), now.Add(30*24*time.Hour))

	srvCert, srvKey, _, _ := ca.issueLeaf(t, dir, "srv", "localhost", "", now.Add(-time.Hour), now.Add(30*24*time.Hour), true)
	cliCert, cliKey, _, _ := ca.issueLeaf(t, dir, "cli", "ingestor", "spiffe://cluster.local/ns/prod/sa/ingestor", now.Add(-time.Hour), now.Add(30*24*time.Hour), false)
	rogueCert, rogueKey, _, _ := rogueCA.issueLeaf(t, dir, "rogue-cli", "rogue", "", now.Add(-time.Hour), now.Add(30*24*time.Hour), false)

	srvReloader, err := NewReloader(PeerConfig{
		Component:  "server",
		CertFile:   srvCert,
		KeyFile:    srvKey,
		CAFile:     ca.caFile,
		ClientAuth: ClientAuthRequire,
	})
	require.NoError(t, err)

	srvTLS, err := srvReloader.ServerTLSConfig()
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	tlsLn := tls.NewListener(ln, srvTLS)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(r.TLS.PeerCertificates[0].Subject.CommonName))
		}),
	}
	go func() { _ = srv.Serve(tlsLn) }()
	defer srv.Close()

	baseURL := "https://" + ln.Addr().String()

	t.Run("valid mTLS client succeeds", func(t *testing.T) {
		cliReloader, err := NewReloader(PeerConfig{
			Component:  "client",
			CertFile:   cliCert,
			KeyFile:    cliKey,
			CAFile:     ca.caFile,
			ServerName: "localhost",
		})
		require.NoError(t, err)

		client := &http.Client{Transport: cliReloader.NewHTTPTransport()}
		resp, err := client.Get(baseURL)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "ingestor", string(body))
	})

	t.Run("missing client certificate is rejected", func(t *testing.T) {
		cliReloader, err := NewReloader(PeerConfig{
			Component:  "client-no-cert",
			CAFile:     ca.caFile,
			ServerName: "localhost",
		})
		require.NoError(t, err)

		client := &http.Client{Transport: cliReloader.NewHTTPTransport()}
		_, err = client.Get(baseURL)
		require.Error(t, err)
	})

	t.Run("client certificate from untrusted CA is rejected", func(t *testing.T) {
		cliReloader, err := NewReloader(PeerConfig{
			Component:  "client-rogue",
			CertFile:   rogueCert,
			KeyFile:    rogueKey,
			CAFile:     ca.caFile,
			ServerName: "localhost",
		})
		require.NoError(t, err)

		client := &http.Client{Transport: cliReloader.NewHTTPTransport()}
		_, err = client.Get(baseURL)
		require.Error(t, err)
	})
}

func TestReloader_LiveRotationAndExpiryWarning(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	ca := newTestCA(t, dir, "ca", now.Add(-time.Hour), now.Add(365*24*time.Hour))

	// First certificate expires in 5 days (within 14-day warning window).
	firstExpiry := now.Add(5 * 24 * time.Hour).Truncate(time.Second)
	certFile, keyFile, _, _ := ca.issueLeaf(t, dir, "active", "localhost", "", now.Add(-time.Hour), firstExpiry, true)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	reloader, err := NewReloader(PeerConfig{
		Component:  "http_server",
		CertFile:   certFile,
		KeyFile:    keyFile,
		CAFile:     ca.caFile,
		ClientAuth: ClientAuthNone,
		Now:        func() time.Time { return now },
		Logger:     logger,
	})
	require.NoError(t, err)
	assert.Contains(t, logBuf.String(), "tls certificate is close to expiration")
	assert.Equal(t, float64(firstExpiry.Unix()), reloader.LeafNotAfterUnix())

	// Rotate on disk to a fresh certificate expiring in 90 days.
	secondExpiry := now.Add(90 * 24 * time.Hour).Truncate(time.Second)
	_, _, newCertPEM, newKeyPEM := ca.issueLeaf(t, dir, "rotated", "localhost", "", now.Add(-time.Hour), secondExpiry, true)
	require.NoError(t, os.WriteFile(certFile, newCertPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, newKeyPEM, 0o600))

	require.NoError(t, reloader.Reload())
	assert.Equal(t, float64(secondExpiry.Unix()), reloader.LeafNotAfterUnix())

	// Corrupt key on disk: Reload returns an error and keeps the previous valid certificate active.
	require.NoError(t, os.WriteFile(keyFile, []byte("not-a-valid-pem"), 0o600))
	require.Error(t, reloader.Reload())
	assert.Equal(t, float64(secondExpiry.Unix()), reloader.LeafNotAfterUnix())
}
