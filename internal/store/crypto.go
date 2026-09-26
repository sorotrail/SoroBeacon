package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ConfigCipher encrypts and decrypts a channel's config JSON so secrets
// (webhook URLs, bot tokens, SMTP credentials) are not stored as plaintext.
// The interface is deliberately small so the algorithm can be swapped later
// without touching the store's channel queries. Implementations must be safe
// for concurrent use and must never put the plaintext or key material in a
// returned error.
type ConfigCipher interface {
	// Encrypt returns an opaque ciphertext that Decrypt can turn back into
	// plaintext. It must not return its input unchanged.
	Encrypt(plaintext []byte) ([]byte, error)
	// Decrypt reverses Encrypt. A wrong key or corrupted ciphertext is an
	// error, never a panic.
	Decrypt(ciphertext []byte) ([]byte, error)
}

// AESGCMCipher is the built-in ConfigCipher: AES-GCM with a fresh random
// nonce prepended to the ciphertext. AES-GCM authenticates the envelope, so
// a wrong key or a tampered row fails Decrypt rather than yielding garbage.
type AESGCMCipher struct {
	aead cipher.AEAD
}

// NewAESGCMCipher builds an AES-GCM cipher from a raw key. key must be 16,
// 24 or 32 bytes (AES-128/192/256). Called at startup so a bad key fails the
// process instead of the first channel write. The error never includes the
// key.
func NewAESGCMCipher(key []byte) (*AESGCMCipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("invalid config encryption key: must be 16, 24 or 32 bytes for AES-GCM (got %d bytes)", len(key))
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init config cipher: %w", err)
	}
	return &AESGCMCipher{aead: aead}, nil
}

// Encrypt seals plaintext with a random nonce. randomising the nonce matters:
// reusing one under the same key would let anyone XOR two ciphertexts
// together and recover both plaintexts.
func (c *AESGCMCipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate config nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens a ciphertext produced by Encrypt. The error is deliberately
// generic: it must not reveal whether the key was wrong or the row was
// tampered with, and must never echo the ciphertext.
func (c *AESGCMCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext is shorter than the nonce")
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, errors.New("decryption failed: wrong key or corrupted ciphertext")
	}
	return plaintext, nil
}

// configEnvelopeVersion identifies the stored envelope format. Bump it if
// the envelope shape changes; an unknown version is rejected as an error
// instead of being mistaken for plaintext.
const configEnvelopeVersion = "v1"

// configEnvelope is the JSON object stored in channels.config once a row has
// been encrypted. "sorobeacon_config" is the one reserved key: a legacy
// plaintext config is a normal object without it, which is how the store
// tells the two apart for the lazy re-encryption path. Keeping the marker to
// a single namespaced key means an ordinary config field called "encrypted"
// can never be mistaken for an envelope. The value is "v1:<base64
// ciphertext>", so the version travels with the data and the column stays
// JSONB.
type configEnvelope struct {
	Config string `json:"sorobeacon_config"`
}

func encodeConfigEnvelope(ciphertext []byte) ([]byte, error) {
	return json.Marshal(configEnvelope{
		Config: configEnvelopeVersion + ":" + base64.StdEncoding.EncodeToString(ciphertext),
	})
}

// parseConfigEnvelope reports whether raw is an encrypted envelope and, if
// so, returns the decoded ciphertext. Plaintext config is not an error — it
// is a row written before a key was configured.
func parseConfigEnvelope(raw []byte) (ciphertext []byte, encrypted bool, err error) {
	var env configEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Config == "" {
		return nil, false, nil // not an envelope: a plaintext object, array or scalar
	}
	version, payload, ok := strings.Cut(env.Config, ":")
	if !ok {
		return nil, false, errors.New("malformed config encryption envelope")
	}
	if version != configEnvelopeVersion {
		return nil, false, fmt.Errorf("unsupported config encryption version %q", version)
	}
	ct, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, false, errors.New("malformed config encryption envelope")
	}
	return ct, true, nil
}

// configForWrite returns the bytes to persist in channels.config: the
// plaintext JSON when no cipher is configured, otherwise an encrypted
// envelope. id is zero on create, so name is preferred in errors. It is a
// free function taking the cipher rather than a method so the Postgres and
// SQLite stores share one implementation of the encryption contract.
func configForWrite(cipher ConfigCipher, id int64, name string, raw json.RawMessage) ([]byte, error) {
	plaintext := jsonOrEmpty(raw)
	if cipher == nil {
		return plaintext, nil
	}
	ciphertext, err := cipher.Encrypt(plaintext)
	if err != nil {
		return nil, fmt.Errorf("%s: encrypt config: %w", channelRef(id, name), err)
	}
	return encodeConfigEnvelope(ciphertext)
}

// configForRead decrypts an encrypted channels.config value, or returns
// legacy plaintext unchanged so rows written before a key was configured keep
// working (they are re-encrypted lazily on the next write). It never includes
// the config contents in an error. Sharing it across backends keeps the
// stored envelope byte-identical between Postgres and SQLite.
func configForRead(cipher ConfigCipher, id int64, name string, raw []byte) (json.RawMessage, error) {
	if cipher == nil {
		return json.RawMessage(raw), nil
	}
	ciphertext, encrypted, err := parseConfigEnvelope(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: read config: %w", channelRef(id, name), err)
	}
	if !encrypted {
		return json.RawMessage(raw), nil
	}
	plaintext, err := cipher.Decrypt(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%s: decrypt config: %w", channelRef(id, name), err)
	}
	return json.RawMessage(plaintext), nil
}

// decryptChannel replaces c.Config with its plaintext form. It is a no-op
// when no cipher is configured.
func decryptChannel(cipher ConfigCipher, c *Channel) error {
	config, err := configForRead(cipher, c.ID, c.Name, c.Config)
	if err != nil {
		return err
	}
	c.Config = config
	return nil
}

// EncryptForStorage encrypts plaintext config for writing to the database,
// using the same envelope as regular channel writes. Nil cipher returns
// plaintext unchanged. Exported for the backup/restore path, which receives
// decrypted config from a backup file and must re-encrypt it for the target
// instance's key.
func EncryptForStorage(cipher ConfigCipher, plaintext json.RawMessage) (json.RawMessage, error) {
	b, err := configForWrite(cipher, 0, "restore", plaintext)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// channelRef names a channel for an error without echoing its config.
func channelRef(id int64, name string) string {
	if name != "" {
		return fmt.Sprintf("channel %q", name)
	}
	return fmt.Sprintf("channel %d", id)
}
