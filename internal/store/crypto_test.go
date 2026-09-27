package store

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testConfigKey is a 32-byte AES-256 key used across the crypto tests.
const testConfigKey = "0123456789abcdef0123456789abcdef"

func testCipher(t *testing.T, key string) *AESGCMCipher {
	t.Helper()
	cp, err := NewAESGCMCipher([]byte(key))
	require.NoError(t, err)
	return cp
}

func TestAESGCMCipherRoundTrip(t *testing.T) {
	c := testCipher(t, testConfigKey)
	plaintext := []byte(`{"url":"https://hooks.example/T000/B000/secret","token":"s3cr3t"}`)

	sealed, err := c.Encrypt(plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, plaintext, sealed)
	assert.NotContains(t, string(sealed), "s3cr3t")

	got, err := c.Decrypt(sealed)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)

	// A fresh nonce per call means sealing the same plaintext twice never
	// produces the same bytes; nonce reuse would leak plaintext via XOR.
	again, err := c.Encrypt(plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, sealed, again)
}

func TestNewAESGCMCipherKeyLengths(t *testing.T) {
	for _, n := range []int{16, 24, 32} {
		_, err := NewAESGCMCipher(bytes.Repeat([]byte("k"), n))
		require.NoError(t, err, "key length %d must be accepted", n)
	}
	for _, n := range []int{0, 15, 17, 31, 33, 64} {
		key := bytes.Repeat([]byte("k"), n)
		_, err := NewAESGCMCipher(key)
		require.Error(t, err, "key length %d must be rejected", n)
		assert.Contains(t, err.Error(), "16, 24 or 32")
		if n > 0 {
			assert.NotContains(t, err.Error(), string(key), "the error must not echo the key")
		}
	}
}

func TestAESGCMCipherDecryptWrongKey(t *testing.T) {
	a := testCipher(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	b := testCipher(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	sealed, err := a.Encrypt([]byte(`{"token":"s3cr3t"}`))
	require.NoError(t, err)

	_, err = b.Decrypt(sealed)
	require.Error(t, err, "a wrong key must be an error, not a panic")
	assert.NotContains(t, err.Error(), "s3cr3t")
	assert.NotContains(t, err.Error(), base64.StdEncoding.EncodeToString(sealed))
}

func TestAESGCMCipherDecryptTruncated(t *testing.T) {
	c := testCipher(t, testConfigKey)
	_, err := c.Decrypt([]byte("short"))
	require.Error(t, err)
}

func TestConfigEnvelopeRoundTrip(t *testing.T) {
	ct := []byte{1, 2, 3, 4}
	raw, err := encodeConfigEnvelope(ct)
	require.NoError(t, err)

	got, encrypted, err := parseConfigEnvelope(raw)
	require.NoError(t, err)
	require.True(t, encrypted)
	assert.Equal(t, ct, got)
}

func TestParseConfigEnvelopePlaintext(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"url":"u"}`, `[]`, `null`, `"x"`, ``,
		// A generic field named "encrypted" must not look like our
		// namespaced envelope.
		`{"encrypted":"gpg"}`, `{"encrypted":true}`, `{"sorobeacon_config":""}`,
	} {
		_, encrypted, err := parseConfigEnvelope([]byte(raw))
		require.NoError(t, err, raw)
		assert.False(t, encrypted, raw)
	}
}

func TestParseConfigEnvelopeUnknownVersion(t *testing.T) {
	_, _, err := parseConfigEnvelope([]byte(`{"sorobeacon_config":"v9:AAAA"}`))
	assert.ErrorContains(t, err, "unsupported")
}
