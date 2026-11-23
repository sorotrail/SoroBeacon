package store

import (
	"context"
	"encoding/json"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretShapedColumnPattern matches column names in SQL DDL that look like
// credentials, keys, tokens, or secrets. Every match must either be listed in
// SensitiveColumnInventory with an explicit protection decision or be a known
// non-secret identifier (such as idempotency_key or key_id).
var secretShapedColumnPattern = regexp.MustCompile(`(?i)^\s*([a-z0-9_]*(?:secret|password|passwd|credential|token|private_key)[a-z0-9_]*)\s+(?:TEXT|JSONB|BYTEA|VARCHAR)`)

var nonSecretColumnAllowlist = map[string]bool{
	"idempotency_keys.idempotency_key": true,
	"monitors.cursor_token":            true,
}

func TestSensitiveColumnInventoryCoversEverySecretShapedMigrationColumn(t *testing.T) {
	t.Parallel()

	inventory := make(map[string]SensitiveColumnDecision, len(SensitiveColumnInventory))
	for _, item := range SensitiveColumnInventory {
		inventory[item.Table+"."+item.Column] = item
		assert.NotEmpty(t, item.Mechanism)
		assert.NotEmpty(t, item.Explanation)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	require.NoError(t, err)

	createTableRe := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z0-9_]+)\s*\(`)
	alterTableRe := regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-z0-9_]+)\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z0-9_]+)`)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		raw, err := fs.ReadFile(migrationsFS, "migrations/"+entry.Name())
		require.NoError(t, err)
		lines := strings.Split(string(raw), "\n")

		currentTable := ""
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "--") {
				continue
			}
			if m := createTableRe.FindStringSubmatch(line); m != nil {
				currentTable = strings.ToLower(m[1])
				continue
			}
			if currentTable != "" && strings.HasPrefix(trimmed, ");") {
				currentTable = ""
				continue
			}
			if m := alterTableRe.FindStringSubmatch(line); m != nil {
				tbl, col := strings.ToLower(m[1]), strings.ToLower(m[2])
				if secretShapedName(col) {
					key := tbl + "." + col
					assert.True(t, isCoveredOrAllowed(key, inventory),
						"migration %s adds secret-shaped column %s without an entry in SensitiveColumnInventory", entry.Name(), key)
				}
				continue
			}
			if currentTable != "" {
				if m := secretShapedColumnPattern.FindStringSubmatch(line); m != nil {
					col := strings.ToLower(m[1])
					key := currentTable + "." + col
					assert.True(t, isCoveredOrAllowed(key, inventory),
						"migration %s defines secret-shaped column %s without an entry in SensitiveColumnInventory", entry.Name(), key)
				}
			}
		}
	}
}

func secretShapedName(col string) bool {
	for _, needle := range []string{"secret", "password", "passwd", "credential", "token", "private_key"} {
		if strings.Contains(col, needle) {
			return true
		}
	}
	return false
}

func isCoveredOrAllowed(key string, inventory map[string]SensitiveColumnDecision) bool {
	if _, ok := inventory[key]; ok {
		return true
	}
	return nonSecretColumnAllowlist[key]
}

func TestKeyRingRotationAndResumableBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := newTestSQLite(t).(*SQLite)

	// 1. Seed legacy plaintext channel config and signing key (cipher == nil).
	ch := &Channel{
		Name:    "ops-webhook",
		Type:    "webhook",
		Config:  json.RawMessage(`{"url":"https://hooks.example.com/secret-hook-token-123"}`),
		Enabled: true,
	}
	require.NoError(t, st.CreateChannel(ctx, ch))

	sk := &ChannelSigningKey{
		ChannelID: ch.ID,
		KeyID:     "csk_legacy",
		Secret:    "whsec_plaintext_secret_value_999",
		IsCurrent: true,
	}
	require.NoError(t, st.CreateChannelSigningKey(ctx, sk))

	// 2. Configure key v1 and run resumable backfill in batches of 1.
	keyV1 := []byte("0123456789abcdef0123456789abcdef")
	cipherV1, err := NewKeyRingCipher(keyV1)
	require.NoError(t, err)
	st.WithConfigCipher(cipherV1)

	rewritten, cursor, err := st.ReencryptSensitiveColumns(ctx, 0, 1)
	require.NoError(t, err)
	assert.Positive(t, rewritten)
	assert.Positive(t, cursor)

	// Verify raw columns in SQLite are now encrypted and contain no plaintext secrets.
	var rawCfg, rawSecret string
	require.NoError(t, st.db.QueryRowContext(ctx, `SELECT config FROM channels WHERE id = ?`, ch.ID).Scan(&rawCfg))
	assert.Contains(t, rawCfg, "sorobeacon_config")
	assert.NotContains(t, rawCfg, "secret-hook-token-123")

	require.NoError(t, st.db.QueryRowContext(ctx, `SELECT secret FROM channel_signing_keys WHERE id = ?`, sk.ID).Scan(&rawSecret))
	assert.True(t, strings.HasPrefix(rawSecret, "enc:v1:"))
	assert.NotContains(t, rawSecret, "whsec_plaintext_secret_value_999")

	// Startup verification passes with the matching key.
	require.NoError(t, st.VerifyEncryptionKey(ctx))

	// 3. Rotate to keyV2 while keeping keyV1 in the keyring: old rows still
	// decrypt without downtime, and a second ReencryptSensitiveColumns pass
	// re-encrypts everything under keyV2.
	keyV2 := []byte("fedcba9876543210fedcba9876543210")
	ringV2V1, err := NewKeyRingCipher(keyV2, keyV1)
	require.NoError(t, err)
	st.WithConfigCipher(ringV2V1)

	gotCh, err := st.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	assert.Contains(t, string(gotCh.Config), "secret-hook-token-123")

	gotKey, err := st.GetChannelSigningKey(ctx, ch.ID, "csk_legacy")
	require.NoError(t, err)
	assert.Equal(t, "whsec_plaintext_secret_value_999", gotKey.Secret)

	// Re-encrypt under keyV2, then drop keyV1 from the ring and confirm rows
	// still open cleanly under keyV2 alone.
	_, _, err = st.ReencryptSensitiveColumns(ctx, 0, 10)
	require.NoError(t, err)

	onlyV2, err := NewKeyRingCipher(keyV2)
	require.NoError(t, err)
	st.WithConfigCipher(onlyV2)
	require.NoError(t, st.VerifyEncryptionKey(ctx))

	// 4. A wrong key fails startup verification and never echoes the secret or
	// returns corrupted ciphertext as a signing secret.
	wrongKey, err := NewKeyRingCipher([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	require.NoError(t, err)
	st.WithConfigCipher(wrongKey)

	err = st.VerifyEncryptionKey(ctx)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-hook-token-123")
	assert.NotContains(t, err.Error(), "whsec_plaintext_secret_value_999")

	_, err = st.GetChannelSigningKey(ctx, ch.ID, "csk_legacy")
	require.Error(t, err, "wrong key on encrypted signing secret must return an error, never ciphertext")
}
