package store

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyDatabaseTLS(t *testing.T) {
	raw := "postgres://sorobeacon:secret@db.internal:5432/sorobeacon?sslmode=disable"
	updated, err := ApplyDatabaseTLS(raw, "verify-full", "/etc/tls/client.crt", "/etc/tls/client.key", "/etc/tls/ca.crt")
	require.NoError(t, err)

	u, err := url.Parse(updated)
	require.NoError(t, err)
	assert.Equal(t, "verify-full", u.Query().Get("sslmode"))
	assert.Equal(t, "/etc/tls/client.crt", u.Query().Get("sslcert"))
	assert.Equal(t, "/etc/tls/client.key", u.Query().Get("sslkey"))
	assert.Equal(t, "/etc/tls/ca.crt", u.Query().Get("sslrootcert"))

	// SQLite URLs are untouched.
	sqliteURL := "sqlite:///var/lib/sorobeacon.db"
	got, err := ApplyDatabaseTLS(sqliteURL, "verify-full", "/a", "/b", "/c")
	require.NoError(t, err)
	assert.Equal(t, sqliteURL, got)
}
