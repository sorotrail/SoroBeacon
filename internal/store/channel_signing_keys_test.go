package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSQLiteStore(t *testing.T) *SQLite {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "keys_test.db")
	require.NoError(t, Migrate(url))
	st, err := NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	return st
}

func TestChannelSigningKeys_Lifecycle(t *testing.T) {
	st := newTestSQLiteStore(t)
	ctx := context.Background()

	// Create channel
	ch := &Channel{Name: "alerts-hook", Type: "webhook", Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, ch))

	// Initial key
	k1 := &ChannelSigningKey{
		ChannelID: ch.ID,
		KeyID:     "csk_key_1",
		Secret:    "secret-alpha-123",
		IsCurrent: true,
	}
	require.NoError(t, st.CreateChannelSigningKey(ctx, k1))
	assert.NotEmpty(t, k1.ID)
	assert.False(t, k1.CreatedAt.IsZero())

	// Get key
	gotK1, err := st.GetChannelSigningKey(ctx, ch.ID, "csk_key_1")
	require.NoError(t, err)
	assert.Equal(t, k1.KeyID, gotK1.KeyID)
	assert.Equal(t, "secret-alpha-123", gotK1.Secret)
	assert.True(t, gotK1.IsCurrent)
	assert.Equal(t, SigningKeyStatusActive, gotK1.Status)

	// Get current key
	curr, err := st.GetCurrentChannelSigningKey(ctx, ch.ID)
	require.NoError(t, err)
	assert.Equal(t, "csk_key_1", curr.KeyID)

	// Rotate key
	k2 := &ChannelSigningKey{
		KeyID:  "csk_key_2",
		Secret: "secret-beta-456",
	}
	require.NoError(t, st.RotateChannelSigningKey(ctx, ch.ID, k2))
	assert.NotEmpty(t, k2.ID)

	// Check new current key
	curr2, err := st.GetCurrentChannelSigningKey(ctx, ch.ID)
	require.NoError(t, err)
	assert.Equal(t, "csk_key_2", curr2.KeyID)
	assert.Equal(t, "secret-beta-456", curr2.Secret)
	assert.True(t, curr2.IsCurrent)

	// Old key must no longer be current, but still active during overlap
	gotK1After, err := st.GetChannelSigningKey(ctx, ch.ID, "csk_key_1")
	require.NoError(t, err)
	assert.False(t, gotK1After.IsCurrent)
	assert.Equal(t, SigningKeyStatusActive, gotK1After.Status)

	// Listing keys: secrets MUST be omitted (never leaked in listings)
	keys, err := st.ListChannelSigningKeys(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, keys, 2)
	for _, k := range keys {
		assert.Empty(t, k.Secret, "signing secrets must never appear in listings")
	}

	// Usage tracking
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, st.RecordChannelSigningKeyUsage(ctx, ch.ID, "csk_key_2", now))
	k2Updated, err := st.GetChannelSigningKey(ctx, ch.ID, "csk_key_2")
	require.NoError(t, err)
	require.NotNil(t, k2Updated.LastUsedAt)

	// Cannot retire the current signing key (must rotate first)
	err = st.RetireChannelSigningKey(ctx, ch.ID, "csk_key_2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot retire active current signing key")

	// Can retire the old key (csk_key_1)
	require.NoError(t, st.RetireChannelSigningKey(ctx, ch.ID, "csk_key_1"))
	k1Retired, err := st.GetChannelSigningKey(ctx, ch.ID, "csk_key_1")
	require.NoError(t, err)
	assert.Equal(t, SigningKeyStatusRetired, k1Retired.Status)

	// Immediate emergency revocation on compromised key
	require.NoError(t, st.RevokeChannelSigningKey(ctx, ch.ID, "csk_key_2"))
	k2Revoked, err := st.GetChannelSigningKey(ctx, ch.ID, "csk_key_2")
	require.NoError(t, err)
	assert.Equal(t, SigningKeyStatusRevoked, k2Revoked.Status)
	assert.False(t, k2Revoked.IsCurrent)
	assert.NotNil(t, k2Revoked.RevokedAt)
}

func TestChannelSigningKeys_WorkspaceIsolation(t *testing.T) {
	st := newTestSQLiteStore(t)
	ctxA := workspace.With(context.Background(), "workspace-a")
	ctxB := workspace.With(context.Background(), "workspace-b")

	// Channel in workspace A
	chA := &Channel{Name: "hook-a", Type: "webhook", Enabled: true}
	require.NoError(t, st.CreateChannel(ctxA, chA))

	// Channel in workspace B
	chB := &Channel{Name: "hook-b", Type: "webhook", Enabled: true}
	require.NoError(t, st.CreateChannel(ctxB, chB))

	// Key in workspace A
	kA := &ChannelSigningKey{
		ChannelID: chA.ID,
		KeyID:     "csk_a",
		Secret:    "secret-a",
		IsCurrent: true,
	}
	require.NoError(t, st.CreateChannelSigningKey(ctxA, kA))

	// Workspace B cannot read Workspace A's key
	_, err := st.GetChannelSigningKey(ctxB, chA.ID, "csk_a")
	require.ErrorIs(t, err, ErrNotFound)

	keysB, err := st.ListChannelSigningKeys(ctxB, chA.ID)
	require.NoError(t, err)
	assert.Empty(t, keysB)

	// Workspace B cannot rotate or retire Workspace A's key
	err = st.RetireChannelSigningKey(ctxB, chA.ID, "csk_a")
	require.Error(t, err)

	err = st.RotateChannelSigningKey(ctxB, chA.ID, &ChannelSigningKey{KeyID: "csk_b_rot", Secret: "sec"})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestChannelSigningKeys_EncryptionAtRest(t *testing.T) {
	st := newTestSQLiteStore(t)
	ctx := context.Background()

	// Install AES-GCM cipher
	key := []byte("01234567890123456789012345678901") // 32 bytes
	cipher, err := NewAESGCMCipher(key)
	require.NoError(t, err)
	st.cipher = cipher

	ch := &Channel{Name: "encrypted-hook", Type: "webhook", Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, ch))

	k := &ChannelSigningKey{
		ChannelID: ch.ID,
		KeyID:     "csk_encrypted",
		Secret:    "super-secret-webhook-key",
		IsCurrent: true,
	}
	require.NoError(t, st.CreateChannelSigningKey(ctx, k))

	// Directly read raw row from sqlite table to assert secret is NOT stored plaintext
	var rawSecret string
	err = st.db.QueryRowContext(ctx, `SELECT secret FROM channel_signing_keys WHERE key_id = 'csk_encrypted'`).Scan(&rawSecret)
	require.NoError(t, err)
	assert.NotEqual(t, "super-secret-webhook-key", rawSecret, "key material must be encrypted at rest")

	// Reading via store decrypts it transparently
	got, err := st.GetChannelSigningKey(ctx, ch.ID, "csk_encrypted")
	require.NoError(t, err)
	assert.Equal(t, "super-secret-webhook-key", got.Secret)
}
