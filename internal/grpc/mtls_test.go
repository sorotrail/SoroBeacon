package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/tlsutil"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func issueGRPCTestCerts(t *testing.T, dir string) (caFile, srvCert, srvKey, mappedCert, mappedKey, unmappedCert, unmappedKey string) {
	t.Helper()
	now := time.Now().UTC()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "grpc-ca"},
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
	caFile = filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))

	makeLeaf := func(name, cn, spiffe string) (string, string) {
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
		if spiffe != "" {
			u, uErr := url.Parse(spiffe)
			require.NoError(t, uErr)
			tmpl.URIs = []*url.URL{u}
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

	srvCert, srvKey = makeLeaf("srv", "localhost", "")
	mappedCert, mappedKey = makeLeaf("mapped", "acme-client", "spiffe://cluster.local/ns/prod/sa/acme-client")
	unmappedCert, unmappedKey = makeLeaf("unmapped", "stranger", "")
	return
}

func TestGRPCServer_MTLSAuthenticationAndWorkspaceScoping(t *testing.T) {
	dir := t.TempDir()
	caFile, srvCert, srvKey, mappedCert, mappedKey, unmappedCert, unmappedKey := issueGRPCTestCerts(t, dir)

	dbURL := "sqlite://" + filepath.Join(dir, "grpc-mtls.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(context.Background(), dbURL)
	require.NoError(t, err)
	defer st.Close()

	err = st.CreateMonitor(workspace.With(context.Background(), workspace.ID("acme")), &store.Monitor{
		Name:        "acme-monitor",
		ContractIDs: []string{"CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"},
		Enabled:     true,
	})
	require.NoError(t, err)
	err = st.CreateMonitor(workspace.With(context.Background(), workspace.Default), &store.Monitor{
		Name:        "default-monitor",
		ContractIDs: []string{"CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"},
		Enabled:     true,
	})
	require.NoError(t, err)

	bindings, err := auth.ParseMTLSPrincipalMap("spiffe://cluster.local/ns/prod/sa/acme-client=acme:viewer")
	require.NoError(t, err)
	authn := auth.New(nil, 0).WithMTLS(true, bindings)

	srvReloader, err := tlsutil.NewReloader(tlsutil.PeerConfig{
		Component:  "grpc_server",
		CertFile:   srvCert,
		KeyFile:    srvKey,
		CAFile:     caFile,
		ClientAuth: tlsutil.ClientAuthRequire,
	})
	require.NoError(t, err)
	srvTLS, err := srvReloader.ServerTLSConfig()
	require.NoError(t, err)

	s := New(st, authn, slog.New(slog.NewTextHandler(io.Discard, nil))).WithTLSConfig(srvTLS)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = s.srv.Serve(lis) }()
	t.Cleanup(func() {
		s.Stop()
		_ = lis.Close()
	})

	t.Run("mapped client certificate authenticates and scopes to acme workspace", func(t *testing.T) {
		cliReloader, err := tlsutil.NewReloader(tlsutil.PeerConfig{
			Component:  "grpc_client",
			CertFile:   mappedCert,
			KeyFile:    mappedKey,
			CAFile:     caFile,
			ServerName: "localhost",
		})
		require.NoError(t, err)

		conn, err := grpc.NewClient(lis.Addr().String(),
			grpc.WithTransportCredentials(credentials.NewTLS(cliReloader.ClientTLSConfig())),
			grpc.WithDefaultCallOptions(grpc.ForceCodec(gobCodec{})),
		)
		require.NoError(t, err)
		defer conn.Close()

		var resp ListMonitorsResponse
		err = conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/ListMonitors", &ListMonitorsRequest{}, &resp)
		require.NoError(t, err)
		require.Len(t, resp.Monitors, 1)
		assert.Equal(t, "acme-monitor", resp.Monitors[0].Name)
	})

	t.Run("unmapped client certificate is denied with PermissionDenied", func(t *testing.T) {
		cliReloader, err := tlsutil.NewReloader(tlsutil.PeerConfig{
			Component:  "grpc_client_unmapped",
			CertFile:   unmappedCert,
			KeyFile:    unmappedKey,
			CAFile:     caFile,
			ServerName: "localhost",
		})
		require.NoError(t, err)

		conn, err := grpc.NewClient(lis.Addr().String(),
			grpc.WithTransportCredentials(credentials.NewTLS(cliReloader.ClientTLSConfig())),
			grpc.WithDefaultCallOptions(grpc.ForceCodec(gobCodec{})),
		)
		require.NoError(t, err)
		defer conn.Close()

		var resp ListMonitorsResponse
		err = conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/ListMonitors", &ListMonitorsRequest{}, &resp)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	})
}
