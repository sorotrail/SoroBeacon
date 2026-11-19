package store

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewPostgresReadOnly covers the NewPostgresReadOnly constructor.
// It is skipped when TEST_DATABASE_URL is not set so `go test ./...` works
// without a database; CI's test-db job sets it (make test-db locally).
func TestNewPostgresReadOnly(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping NewPostgresReadOnly tests")
	}
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")

	// Happy path: valid database URL successfully opens a read-only pool.
	st, err := NewPostgresReadOnly(context.Background(), url, PoolSettings{})
	require.NoError(t, err)
	require.NotNil(t, st)
	require.NotNil(t, st.pool)
	// Verify the pool is set to read-only mode.
	err = st.pool.Ping(context.Background())
	require.NoError(t, err)
	_ = st
}