package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSQLiteStoreForTokens(t *testing.T) *SQLite {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "tokens_test.db")
	require.NoError(t, Migrate(url))
	st, err := NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	return st
}

func TestAPIToken_RotationLifecycle(t *testing.T) {
	st := newTestSQLiteStoreForTokens(t)
	ctx := workspace.With(context.Background(), "acme")

	// 1. Create predecessor token with no expiry
	pred := &auth.Token{
		Name:   "primary-agent",
		Prefix: "sb_12345678",
		Hash:   "a1b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef0",
		Scopes: []auth.Scope{auth.ScopeMonitorsRead, auth.ScopeMonitorsWrite},
	}
	require.NoError(t, st.CreateAPIToken(ctx, pred))
	require.NotEmpty(t, pred.ID)

	// 2. Rotate token with overlap expiry
	overlapExpiry := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	succ := &auth.Token{
		Name:   "primary-agent-successor",
		Prefix: "sb_87654321",
		Hash:   "b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef01a",
		Scopes: []auth.Scope{auth.ScopeMonitorsRead, auth.ScopeMonitorsWrite},
	}

	require.NoError(t, st.RotateAPIToken(ctx, pred.ID, succ, overlapExpiry))
	assert.NotEmpty(t, succ.ID)
	assert.Equal(t, &pred.ID, succ.RotatedFromID)
	assert.False(t, succ.RotatedAt.IsZero())

	// 3. Verify predecessor in store
	predAfter, err := st.GetAPIToken(ctx, pred.ID)
	require.NoError(t, err)
	assert.Equal(t, &succ.ID, predAfter.SuccessorID)
	assert.False(t, predAfter.RotatedAt.IsZero())
	assert.Equal(t, overlapExpiry, predAfter.ExpiresAt)

	// 4. Verify successor in store
	succAfter, err := st.GetAPIToken(ctx, succ.ID)
	require.NoError(t, err)
	assert.Equal(t, &pred.ID, succAfter.RotatedFromID)
	assert.Nil(t, succAfter.SuccessorID)

	// 5. Abandon rotation via successor ID
	require.NoError(t, st.AbandonAPITokenRotation(ctx, succ.ID))

	// Successor must now be revoked
	succRevoked, err := st.GetAPIToken(ctx, succ.ID)
	require.NoError(t, err)
	assert.False(t, succRevoked.RevokedAt.IsZero(), "successor should be revoked after abandon")

	// Predecessor must be restored to no expiry and no successor
	predRestored, err := st.GetAPIToken(ctx, pred.ID)
	require.NoError(t, err)
	assert.Nil(t, predRestored.SuccessorID, "predecessor successor_id must be cleared")
	assert.True(t, predRestored.RotatedAt.IsZero(), "predecessor rotated_at must be cleared")
	assert.True(t, predRestored.ExpiresAt.IsZero(), "predecessor expiry should be restored to zero")
}

func TestAPIToken_RotationWorkspaceIsolation(t *testing.T) {
	st := newTestSQLiteStoreForTokens(t)
	acmeCtx := workspace.With(context.Background(), "acme")
	otherCtx := workspace.With(context.Background(), "other")

	pred := &auth.Token{
		Name:   "acme-token",
		Prefix: "sb_acme0001",
		Hash:   "1111111111111111111111111111111111111111111111111111111111111111",
		Scopes: []auth.Scope{auth.ScopeMonitorsRead},
	}
	require.NoError(t, st.CreateAPIToken(acmeCtx, pred))

	// Attempt to rotate acme token from other workspace must fail
	succ := &auth.Token{
		Name:   "intruder",
		Prefix: "sb_other001",
		Hash:   "2222222222222222222222222222222222222222222222222222222222222222",
		Scopes: []auth.Scope{auth.ScopeMonitorsRead},
	}
	err := st.RotateAPIToken(otherCtx, pred.ID, succ, time.Now().Add(24*time.Hour))
	assert.Error(t, err)

	// Rotate legitimately
	require.NoError(t, st.RotateAPIToken(acmeCtx, pred.ID, succ, time.Now().Add(24*time.Hour)))

	// Attempt to abandon from other workspace must fail
	assert.Error(t, st.AbandonAPITokenRotation(otherCtx, pred.ID))
}
