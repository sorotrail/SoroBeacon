package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterTLSCertificateExpiry(t *testing.T) {
	m := New()
	var ts atomic.Int64
	ts.Store(1800000000)

	m.RegisterTLSCertificateExpiry("server", func() float64 {
		return float64(ts.Load())
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), `sorobeacon_tls_certificate_not_after_timestamp_seconds{component="server"} 1.8e+09`)

	// Simulate SIGHUP reload updating the leaf NotAfter timestamp.
	ts.Store(1900000000)
	rec2 := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec2, req)
	body2, err := io.ReadAll(rec2.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body2), `sorobeacon_tls_certificate_not_after_timestamp_seconds{component="server"} 1.9e+09`)
}
