package api

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestAuthMiddleware_MTLSPrincipalMapping(t *testing.T) {
	bindings, err := auth.ParseMTLSPrincipalMap("cn:acme-ingestor=acme:editor")
	require.NoError(t, err)

	authN := auth.New(nil, 0).WithMTLS(true, bindings)
	mw := AuthMiddleware(authN, nil)

	var seenWorkspace workspace.ID
	var seenRole auth.Role
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenWorkspace, _ = workspace.From(r.Context())
		seenRole, _ = auth.RoleFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// Mapped client cert -> 200 OK with workspace=acme, role=editor.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: "acme-ingestor"}},
		},
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, workspace.ID("acme"), seenWorkspace)
	assert.Equal(t, auth.RoleEditor, seenRole)

	// Unmapped client cert -> 403 Forbidden.
	reqUnmapped := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
	reqUnmapped.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: "stranger"}},
		},
	}
	recUnmapped := httptest.NewRecorder()
	handler.ServeHTTP(recUnmapped, reqUnmapped)
	assert.Equal(t, http.StatusForbidden, recUnmapped.Code)

	// No client cert and no bearer token -> 401 Unauthorized.
	reqAnon := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
	recAnon := httptest.NewRecorder()
	handler.ServeHTTP(recAnon, reqAnon)
	assert.Equal(t, http.StatusUnauthorized, recAnon.Code)
}
