package tlsutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HasCertificate is the guard ServerTLSConfig relies on, so it has to be true
// only when a leaf certificate/key pair is actually loaded: a CA-only client
// reloader and a nil receiver must both report false rather than building a
// server config without a certificate.
func TestHasCertificate(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	ca := newTestCA(t, dir, "root", now.Add(-24*time.Hour), now.Add(365*24*time.Hour))
	certFile, keyFile, _, _ := ca.issueLeaf(t, dir, "server", "localhost", "",
		now.Add(-time.Hour), now.Add(30*24*time.Hour), true)

	tests := []struct {
		name     string
		cfg      PeerConfig
		nil      bool
		wantCert bool
	}{
		{
			name: "loaded certificate and key reports true",
			cfg: PeerConfig{
				CertFile: certFile,
				KeyFile:  keyFile,
				Now:      func() time.Time { return now },
			},
			wantCert: true,
		},
		{
			name: "ca-only reloader reports false",
			cfg: PeerConfig{
				CAFile: ca.caFile,
				Now:    func() time.Time { return now },
			},
			wantCert: false,
		},
		{
			name:     "nil reloader reports false",
			nil:      true,
			wantCert: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.nil {
				var r *Reloader
				assert.False(t, r.HasCertificate())
				return
			}

			r, err := NewReloader(tc.cfg)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCert, r.HasCertificate())
		})
	}
}
