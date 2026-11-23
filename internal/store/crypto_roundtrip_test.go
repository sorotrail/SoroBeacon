package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Postgres-backed tests for the encrypted channel config round trip: write a
// channel, read it back, and inspect what actually landed in the column. They
// use the shared testStore helper (postgres_test.go), so the suite skips
// cleanly unless TEST_DATABASE_URL points at a database.
//
// The assertion that matters most is deliberately blunt: read channels.config
// straight from the table and check the plaintext secret is not in it. That is
// the property that keeps credentials out of a database dump, and it breaks the
// moment encryption is dropped from the write path.
//
// Every credential fixture is an obviously fake value ("not-a-real-...") so a
// leak can never be mistaken for a real one.

// TestChannelConfigRoundTripEncrypted writes a channel with a known config and
// checks both halves of the round trip: the store returns the config unchanged,
// and the raw column contains an encrypted envelope rather than the secret.
func TestChannelConfigRoundTripEncrypted(t *testing.T) {
	st := testStore(t).WithConfigCipher(testCipher(t, testConfigKey))
	ctx := context.Background()

	const (
		fakeWebhookURL = "https://hooks.example.invalid/not-a-real-webhook-id"
		fakeBotToken   = "not-a-real-token-0000"
	)
	cfg := json.RawMessage(`{"webhook_url":"` + fakeWebhookURL + `","bot_token":"` + fakeBotToken + `"}`)

	c := &Channel{Name: "round-trip", Type: "webhook", Config: cfg, Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))

	// Reading through the store decrypts transparently and returns exactly
	// what was written.
	got, err := st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(cfg), string(got.Config))

	// The raw column must be an envelope holding no plaintext secret. This is
	// the check that fails if someone disables encryption.
	raw := string(rawChannelConfig(t, st, c.ID))
	assert.NotContains(t, raw, fakeWebhookURL, "the stored column must not contain the webhook URL")
	assert.NotContains(t, raw, fakeBotToken, "the stored column must not contain the bot token")
	assert.Contains(t, raw, "sorobeacon_config", "the row must be recognisable as an encrypted envelope")
}

// TestChannelConfigUpdateReencrypts updates a channel with a new config and
// proves the row was rewritten: the ciphertext changes and the replaced secret
// is gone from the column.
func TestChannelConfigUpdateReencrypts(t *testing.T) {
	st := testStore(t).WithConfigCipher(testCipher(t, testConfigKey))
	ctx := context.Background()

	const (
		fakeWebhookURL = "https://hooks.example.invalid/not-a-real-webhook-id"
		oldToken       = "not-a-real-token-old"
		newToken       = "not-a-real-token-rotated"
	)
	c := &Channel{
		Name: "rotate", Type: "webhook", Enabled: true,
		Config: json.RawMessage(`{"webhook_url":"` + fakeWebhookURL + `","bot_token":"` + oldToken + `"}`),
	}
	require.NoError(t, st.CreateChannel(ctx, c))
	before := rawChannelConfig(t, st, c.ID)

	c.Config = json.RawMessage(`{"webhook_url":"` + fakeWebhookURL + `","bot_token":"` + newToken + `"}`)
	require.NoError(t, st.UpdateChannel(ctx, c))
	after := rawChannelConfig(t, st, c.ID)

	// Different bytes prove the update wrote a fresh row rather than leaving
	// the previous ciphertext in place. The read-back below pins the new
	// secret, so this cannot pass by accident.
	require.NotEqual(t, before, after, "update must write new ciphertext")
	assert.NotContains(t, string(after), oldToken, "the replaced secret must not linger in the row")
	assert.NotContains(t, string(after), newToken)
	assert.Contains(t, string(after), "sorobeacon_config")

	got, err := st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"webhook_url":"`+fakeWebhookURL+`","bot_token":"`+newToken+`"}`,
		string(got.Config))
}

// TestChannelConfigWrongKeyFailsClearly covers a key rotation done without
// re-encrypting: the stored row becomes undecryptable, and the store must say
// so instead of handing the ciphertext back as if it were config.
func TestChannelConfigWrongKeyFailsClearly(t *testing.T) {
	st := testStore(t).WithConfigCipher(testCipher(t, testConfigKey))
	ctx := context.Background()

	const fakeBotToken = "not-a-real-token-wrong-key"
	c := &Channel{
		Name: "wrong-key", Type: "webhook", Enabled: true,
		Config: json.RawMessage(`{"bot_token":"` + fakeBotToken + `"}`),
	}
	require.NoError(t, st.CreateChannel(ctx, c))
	rawBefore := string(rawChannelConfig(t, st, c.ID))

	// A different 32-byte key cannot open the envelope.
	st.WithConfigCipher(testCipher(t, "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"))

	got, err := st.GetChannel(ctx, c.ID)
	require.Error(t, err, "a wrong key must fail rather than return undecryptable bytes")
	assert.Nil(t, got, "no config may be returned when decryption fails")
	assert.Contains(t, err.Error(), "decrypt config")
	assert.Contains(t, err.Error(), "wrong-key", "the error must name the channel so an operator can find it")
	assert.NotContains(t, err.Error(), fakeBotToken, "the error must not echo the plaintext")
	assert.NotContains(t, err.Error(), rawBefore, "the error must not echo the stored ciphertext")
}

// TestChannelConfigUnicodeAndQuotesRoundTrip guards the encoding boundary: a
// config with multi-byte characters and embedded quotes must survive encrypt,
// store, read and decrypt unchanged.
func TestChannelConfigUnicodeAndQuotesRoundTrip(t *testing.T) {
	st := testStore(t).WithConfigCipher(testCipher(t, testConfigKey))
	ctx := context.Background()

	// Marshal from a value so the quotes and backslash are escaped correctly;
	// the JSON text produced here is what has to come back byte-for-byte.
	cfg, err := json.Marshal(map[string]any{
		"webhook_url": "https://hooks.example.invalid/not-a-real-unicode-hook",
		"note":        "caf\u00e9 \u2014 na\u00efve \u2014 \u65e5\u672c\u8a9e \u2014 \u03a9",
		"bot_token":   `not-a-real-token-with-"quotes"-and-a-backslash-\`,
	})
	require.NoError(t, err)

	c := &Channel{Name: "unicode", Type: "webhook", Config: cfg, Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, c))

	got, err := st.GetChannel(ctx, c.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(cfg), string(got.Config), "multi-byte text and embedded quotes must round-trip exactly")

	// The multi-byte text must not be readable in the stored column either.
	raw := string(rawChannelConfig(t, st, c.ID))
	assert.NotContains(t, raw, "not-a-real-token")
	assert.NotContains(t, raw, "\u65e5\u672c\u8a9e")
}
