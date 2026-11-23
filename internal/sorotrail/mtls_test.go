package sorotrail

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/tlsutil"
)

func TestNewClientWithTLS_MTLSUpstream(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sorotrail-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	caFile := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))

	makeLeaf := func(name, cn string) (string, string) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			DNSNames:     []string{"localhost"},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &k.PublicKey, caKey)
		require.NoError(t, err)
		kDER, err := x509.MarshalECPrivateKey(k)
		require.NoError(t, err)
		cf := filepath.Join(dir, name+".crt")
		kf := filepath.Join(dir, name+".key")
		require.NoError(t, os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
		require.NoError(t, os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kDER}), 0o600))
		return cf, kf
	}

	srvCert, srvKey := makeLeaf("srv", "localhost")
	cliCert, cliKey := makeLeaf("cli", "sorobeacon-poller")

	srvReloader, err := tlsutil.NewReloader(tlsutil.PeerConfig{
		Component:  "upstream_sorotrail",
		CertFile:   srvCert,
		KeyFile:    srvKey,
		CAFile:     caFile,
		ClientAuth: tlsutil.ClientAuthRequire,
	})
	require.NoError(t, err)
	srvTLS, err := srvReloader.ServerTLSConfig()
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	var seenClientCN string
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
				seenClientCN = r.TLS.PeerCertificates[0].Subject.CommonName
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total_events":42,"oldest_stored_ledger":100,"last_ingested_ledger":200}`))
		}),
	}
	go func() { _ = srv.Serve(tls.NewListener(ln, srvTLS)) }()
	defer srv.Close()

	cliReloader, err := tlsutil.NewReloader(tlsutil.PeerConfig{
		Component:  "sorotrail_client",
		CertFile:   cliCert,
		KeyFile:    cliKey,
		CAFile:     caFile,
		ServerName: "localhost",
	})
	require.NoError(t, err)

	client := NewClientWithTLS("https://"+ln.Addr().String(), cliReloader)
	stats, err := client.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(42), stats.TotalEvents)
	assert.Equal(t, "sorobeacon-poller", seenClientCN)
}
