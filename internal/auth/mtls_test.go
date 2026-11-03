package auth

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestParseMTLSPrincipalMap(t *testing.T) {
	bindings, err := ParseMTLSPrincipalMap("spiffe://cluster.local/ns/prod/sa/ingestor=acme:editor, cn:ops-admin=default:admin, worker.internal=beta")
	require.NoError(t, err)
	require.Len(t, bindings, 3)

	assert.Equal(t, MTLSBinding{
		Subject:   "spiffe://cluster.local/ns/prod/sa/ingestor",
		Workspace: workspace.ID("acme"),
		Role:      RoleEditor,
	}, bindings[0])
	assert.Equal(t, MTLSBinding{
		Subject:   "cn:ops-admin",
		Workspace: workspace.Default,
		Role:      RoleAdmin,
	}, bindings[1])
	assert.Equal(t, MTLSBinding{
		Subject:   "worker.internal",
		Workspace: workspace.ID("beta"),
		Role:      RoleAdmin,
	}, bindings[2])

	t.Run("rejects malformed entries", func(t *testing.T) {
		_, err := ParseMTLSPrincipalMap("invalid-entry")
		require.Error(t, err)

		_, err = ParseMTLSPrincipalMap("cn:ops=acme:superuser")
		require.Error(t, err)

		_, err = ParseMTLSPrincipalMap("cn:ops=BAD WORKSPACE:admin")
		require.Error(t, err)

		_, err = ParseMTLSPrincipalMap("cn:ops=acme:viewer,CN:OPS=beta:admin")
		require.Error(t, err)
	})
}

func TestAuthenticator_MTLSRequestResolution(t *testing.T) {
	spiffeURI, err := url.Parse("spiffe://cluster.local/ns/prod/sa/ingestor")
	require.NoError(t, err)

	bindings, err := ParseMTLSPrincipalMap("spiffe://cluster.local/ns/prod/sa/ingestor=acme:editor,cn:viewer-client=beta:viewer")
	require.NoError(t, err)

	authN := New(nil, 0).WithMTLS(true, bindings)
	require.True(t, authN.Enabled())

	t.Run("SPIFFE URI SAN maps to configured workspace and role", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
		req.TLS = &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{
				{
					Subject: pkix.Name{CommonName: "ingestor"},
					URIs:    []*url.URL{spiffeURI},
				},
			},
		}
		p, status := authN.AuthenticateRequest(req)
		require.Equal(t, AuthOK, status)
		require.NotNil(t, p)
		assert.Equal(t, workspace.ID("acme"), p.Workspace)
		assert.Equal(t, RoleEditor, p.Role)
	})

	t.Run("CommonName maps to configured workspace and role", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
		req.TLS = &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{
				{
					Subject: pkix.Name{CommonName: "viewer-client"},
				},
			},
		}
		p, status := authN.AuthenticateRequest(req)
		require.Equal(t, AuthOK, status)
		require.NotNil(t, p)
		assert.Equal(t, workspace.ID("beta"), p.Workspace)
		assert.Equal(t, RoleViewer, p.Role)
	})

	t.Run("unmapped client certificate returns AuthForbidden when bindings exist", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
		req.TLS = &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{
				{
					Subject: pkix.Name{CommonName: "unknown-service"},
				},
			},
		}
		p, status := authN.AuthenticateRequest(req)
		assert.Equal(t, AuthForbidden, status)
		assert.Nil(t, p)
	})

	t.Run("missing client certificate returns AuthUnauthenticated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
		p, status := authN.AuthenticateRequest(req)
		assert.Equal(t, AuthUnauthenticated, status)
		assert.Nil(t, p)
	})
}
