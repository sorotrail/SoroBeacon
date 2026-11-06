package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTestCertFiles(t *testing.T, dir string, expired bool) (caFile, certFile, keyFile string) {
	t.Helper()
	now := time.Now().UTC()
	notBefore := now.Add(-time.Hour)
	notAfter := now.Add(30 * 24 * time.Hour)
	if expired {
		notBefore = now.Add(-48 * time.Hour)
		notAfter = now.Add(-time.Hour)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             now.Add(-48 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	require.NoError(t, err)
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	require.NoError(t, err)

	caFile = filepath.Join(dir, "ca.pem")
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}), 0o600))
	return caFile, certFile, keyFile
}

func TestLoadMTLS_ValidAndInvalidConfigurations(t *testing.T) {
	dir := t.TempDir()
	caFile, certFile, keyFile := writeTestCertFiles(t, dir, false)
	_, expCertFile, expKeyFile := writeTestCertFiles(t, t.TempDir(), true)

	t.Run("valid server, sorotrail client, and database mTLS configuration", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
		t.Setenv("TLS_CERT_FILE", certFile)
		t.Setenv("TLS_KEY_FILE", keyFile)
		t.Setenv("TLS_CLIENT_CA_FILE", caFile)
		t.Setenv("TLS_CLIENT_AUTH", "require")
		t.Setenv("MTLS_PRINCIPAL_MAP", "spiffe://cluster.local/ns/prod/sa/ingestor=acme:editor")
		t.Setenv("SOROTRAIL_TLS_CERT_FILE", certFile)
		t.Setenv("SOROTRAIL_TLS_KEY_FILE", keyFile)
		t.Setenv("SOROTRAIL_TLS_CA_FILE", caFile)
		t.Setenv("SOROTRAIL_TLS_SERVER_NAME", "localhost")
		t.Setenv("DATABASE_SSL_MODE", "verify-full")
		t.Setenv("DATABASE_SSL_CERT_FILE", certFile)
		t.Setenv("DATABASE_SSL_KEY_FILE", keyFile)
		t.Setenv("DATABASE_SSL_ROOT_CERT_FILE", caFile)

		cfg, problems := LoadProblems()
		require.Empty(t, problems)
		assert.True(t, cfg.ServerTLSEnabled())
		assert.True(t, cfg.ServerMTLSEnabled())
		assert.True(t, cfg.SoroTrailTLSEnabled())
		assert.True(t, cfg.DatabaseTLSEnabled())
		require.Len(t, cfg.MTLSBindings, 1)
		assert.Equal(t, "acme", string(cfg.MTLSBindings[0].Workspace))
	})

	t.Run("missing TLS_KEY_FILE fails startup", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
		t.Setenv("TLS_CERT_FILE", certFile)
		t.Setenv("TLS_KEY_FILE", "")

		_, problems := LoadProblems()
		require.NotEmpty(t, problems)
		assert.Equal(t, "TLS_KEY_FILE", problems[0].Field)
	})

	t.Run("expired TLS_CERT_FILE fails startup", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
		t.Setenv("TLS_CERT_FILE", expCertFile)
		t.Setenv("TLS_KEY_FILE", expKeyFile)

		_, problems := LoadProblems()
		require.NotEmpty(t, problems)
		assert.Equal(t, "TLS_CERT_FILE", problems[0].Field)
		assert.Contains(t, problems[0].Reason, "expired")
	})

	t.Run("DATABASE_SSL_MODE rejected on sqlite DATABASE_URL", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "sqlite:///tmp/test.db")
		t.Setenv("DATABASE_SSL_MODE", "require")

		_, problems := LoadProblems()
		require.NotEmpty(t, problems)
		assert.Equal(t, "DATABASE_SSL_MODE", problems[0].Field)
	})
}
