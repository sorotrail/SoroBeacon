package lease

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewPostgresHappyPath verifies that NewPostgres successfully creates a
// lease when given a valid PostgreSQL DATABASE_URL.
func TestNewPostgresHappyPath(t *testing.T) {
	// Use a minimal valid postgres URL; the actual connection is lazy so this
	// just exercises the URL parsing and config building paths.
	_, err := NewPostgres("postgres://127.0.0.1:5432/sorobeacon", Options{}, testLogger())
	require.NoError(t, err)
}

// TestNewPostgresErrorPath verifies that NewPostgres returns an error when
// given a non-PostgreSQL DATABASE_URL (e.g., sqlite or mysql).
func TestNewPostgresErrorPath(t *testing.T) {
	_, err := NewPostgres("sqlite:///tmp/test.db", Options{}, testLogger())
	require.Error(t, err)
	require.Contains(t, err.Error(), "leader election needs a postgres")
}

// TestNewPostgresErrorPathMalformedURL verifies that NewPostgres returns an
// error when given a malformed DATABASE_URL.
func TestNewPostgresErrorPathMalformedURL(t *testing.T) {
	_, err := NewPostgres("not-a-url", Options{}, testLogger())
	require.Error(t, err)
}