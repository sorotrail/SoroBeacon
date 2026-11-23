package tlsutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLeafDaysRemaining(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)

	t.Run("nil receiver returns 0", func(t *testing.T) {
		var r *Reloader
		assert.Equal(t, float64(0), r.LeafDaysRemaining(now))
	})

	t.Run("reloader without leaf cert returns 0", func(t *testing.T) {
		dir := t.TempDir()
		ca := newTestCA(t, dir, "root", now.Add(-24*time.Hour), now.Add(365*24*time.Hour))
		r, err := NewReloader(PeerConfig{
			CAFile: ca.caFile,
			Now:    func() time.Time { return now },
		})
		require.NoError(t, err)
		assert.Equal(t, float64(0), r.LeafDaysRemaining(now))
	})

	t.Run("returns positive days for a valid leaf cert", func(t *testing.T) {
		dir := t.TempDir()
		ca := newTestCA(t, dir, "root", now.Add(-24*time.Hour), now.Add(365*24*time.Hour))
		certFile, keyFile, _, _ := ca.issueLeaf(t, dir, "server", "server", "",
			now.Add(-time.Hour), now.Add(30*24*time.Hour), true)

		r, err := NewReloader(PeerConfig{
			CertFile: certFile,
			KeyFile:  keyFile,
			Now:      func() time.Time { return now },
		})
		require.NoError(t, err)

		days := r.LeafDaysRemaining(now)
		assert.Greater(t, days, float64(0), "want positive days for a cert that expires in 30 days")
		assert.InDelta(t, 30.0, days, 1.0, "want ~30 days remaining")
	})

	t.Run("returns negative days when queried past the cert expiry", func(t *testing.T) {
		dir := t.TempDir()
		ca := newTestCA(t, dir, "root", now.Add(-24*time.Hour), now.Add(365*24*time.Hour))
		// Cert valid at construction time (now), but expires in 1 hour.
		expiry := now.Add(time.Hour)
		certFile, keyFile, _, _ := ca.issueLeaf(t, dir, "short", "short", "",
			now.Add(-time.Minute), expiry, true)

		r, err := NewReloader(PeerConfig{
			CertFile: certFile,
			KeyFile:  keyFile,
			Now:      func() time.Time { return now },
		})
		require.NoError(t, err)

		// Query well past the expiry: days should be negative.
		future := expiry.Add(24 * time.Hour)
		days := r.LeafDaysRemaining(future)
		assert.Less(t, days, float64(0), "want negative days when queried after the cert has expired")
	})

	t.Run("zero time falls back to the reloader clock", func(t *testing.T) {
		dir := t.TempDir()
		ca := newTestCA(t, dir, "root", now.Add(-24*time.Hour), now.Add(365*24*time.Hour))
		certFile, keyFile, _, _ := ca.issueLeaf(t, dir, "server", "server", "",
			now.Add(-time.Hour), now.Add(60*24*time.Hour), true)

		r, err := NewReloader(PeerConfig{
			CertFile: certFile,
			KeyFile:  keyFile,
			Now:      func() time.Time { return now },
		})
		require.NoError(t, err)

		days := r.LeafDaysRemaining(time.Time{})
		assert.InDelta(t, 60.0, days, 1.0, "zero time should use the reloader's injected clock")
	})
}
