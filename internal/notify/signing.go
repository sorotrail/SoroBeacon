package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// DefaultMaxSignatureSkew is the maximum tolerated clock skew between
// delivery generation and verification (5 minutes).
const DefaultMaxSignatureSkew = 5 * time.Minute

// CanonicalString builds the canonical byte slice to sign.
// The signature covers:
//   - keyID (when present)
//   - timestamp
//   - idempotencyKey (when present)
//   - rawBody
//
// Separated by dots ('.'). When keyID and idempotencyKey are empty (legacy mode),
// it produces the exact historical format "<timestamp>.<rawBody>".
// Because the key identifier and idempotency headers are incorporated into the signed
// bytes, an attacker cannot modify them in flight to steer the delivery to a weaker key
// or alter deduplication semantics.
func CanonicalString(keyID, timestamp, idempotencyKey string, rawBody []byte) []byte {
	var b bytes.Buffer
	if keyID != "" {
		b.WriteString(keyID)
		b.WriteByte('.')
	}
	b.WriteString(timestamp)
	b.WriteByte('.')
	if idempotencyKey != "" {
		b.WriteString(idempotencyKey)
		b.WriteByte('.')
	}
	b.Write(rawBody)
	return b.Bytes()
}

// Sign computes the HMAC-SHA256 signature for legacy payloads ("<timestamp>.<rawBody>").
// Preserved for full backwards compatibility.
func Sign(key []byte, timestamp string, rawBody []byte) string {
	return SignCanonical(key, "", timestamp, "", rawBody)
}

// SignCanonical computes the hex-encoded HMAC-SHA256 signature over the canonical string.
func SignCanonical(key []byte, keyID, timestamp, idempotencyKey string, rawBody []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(CanonicalString(keyID, timestamp, idempotencyKey, rawBody))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyOptions holds the fields required to verify a webhook delivery.
type VerifyOptions struct {
	KeyID          string
	Timestamp      string
	IdempotencyKey string
	Body           []byte
	Signature      string
	MaxSkew        time.Duration
}

// VerifySignature verifies a delivery using constant-time comparison against a set of
// candidate keys (map of keyID -> secret bytes).
//
// It returns the key ID that successfully verified the signature, or "" if verification failed.
// All comparisons use constant-time operations to prevent timing side channels.
func VerifySignature(keys map[string][]byte, opts VerifyOptions) (string, bool) {
	if len(keys) == 0 || opts.Signature == "" || opts.Timestamp == "" {
		return "", false
	}

	// Replay / skew protection
	if opts.MaxSkew > 0 {
		ts, err := strconv.ParseInt(opts.Timestamp, 10, 64)
		if err != nil {
			return "", false
		}
		deliveryTime := time.Unix(ts, 0)
		diff := time.Since(deliveryTime)
		if diff < 0 {
			diff = -diff
		}
		if diff > opts.MaxSkew {
			return "", false
		}
	}

	sigBytes := []byte(opts.Signature)

	// 1. If opts.KeyID is specified, select that key directly.
	if opts.KeyID != "" {
		k, ok := keys[opts.KeyID]
		if !ok {
			return "", false
		}
		expected := SignCanonical(k, opts.KeyID, opts.Timestamp, opts.IdempotencyKey, opts.Body)
		if hmac.Equal([]byte(expected), sigBytes) {
			return opts.KeyID, true
		}
		return "", false
	}

	// 2. Legacy delivery without KeyID: constant-time check over candidate keys.
	var matchedID string
	matched := false

	// Deterministic ordering for constant-time sweep
	keyIDs := make([]string, 0, len(keys))
	for id := range keys {
		keyIDs = append(keyIDs, id)
	}
	sort.Strings(keyIDs)

	for _, id := range keyIDs {
		k := keys[id]
		// Try canonical string with key ID
		exp1 := SignCanonical(k, id, opts.Timestamp, opts.IdempotencyKey, opts.Body)
		if subtle.ConstantTimeCompare([]byte(exp1), sigBytes) == 1 {
			if !matched {
				matchedID = id
				matched = true
			}
		}

		// Try legacy format without key ID
		exp2 := SignCanonical(k, "", opts.Timestamp, "", opts.Body)
		if subtle.ConstantTimeCompare([]byte(exp2), sigBytes) == 1 {
			if !matched {
				matchedID = id
				matched = true
			}
		}
	}

	return matchedID, matched
}

// GenerateSigningKeyID generates a random key identifier with a "csk_" prefix.
func GenerateSigningKeyID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("csk_%s", hex.EncodeToString(b))
}

// GenerateSigningKeySecret generates 256 bits of cryptographically secure random key material.
func GenerateSigningKeySecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SigningKey represents a single signing key for a webhook channel.
type SigningKey struct {
	ID         string     `json:"id" db:"id"`
	Key        []byte     `json:"-" db:"key"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
	IsCurrent  bool       `json:"is_current" db:"is_current"`
	IsActive   bool       `json:"is_active" db:"is_active"`
	IsExpired  bool       `json:"is_expired" db:"is_expired"`
	Status     string     `json:"status" db:"status"` // "active", "retired", "revoked"
	LastUsedAt *time.Time `json:"last_used_at,omitempty" db:"last_used_at"`
}

// KeyTable manages per-channel active and historical signing keys in memory.
type KeyTable struct {
	mu       sync.Mutex
	channels map[string]map[string]*SigningKey // URL -> keyID -> SigningKey
}

// NewKeyTable returns an empty key table.
func NewKeyTable() *KeyTable {
	return &KeyTable{channels: make(map[string]map[string]*SigningKey)}
}

// AddKey adds a new signing key for a channel identified by its webhook URL.
func (kt *KeyTable) AddKey(url string, key []byte) (*SigningKey, error) {
	kt.mu.Lock()
	defer kt.mu.Unlock()

	if kt.channels[url] == nil {
		kt.channels[url] = make(map[string]*SigningKey)
	}

	id := GenerateSigningKeyID()
	// Demote any existing current key
	for _, existing := range kt.channels[url] {
		existing.IsCurrent = false
	}

	k := &SigningKey{
		ID:        id,
		Key:       key,
		CreatedAt: time.Now(),
		IsCurrent: true,
		IsActive:  true,
		Status:    "active",
	}

	kt.channels[url][id] = k
	return k, nil
}

// GetCurrentKey returns the active current signing key for a channel.
func (kt *KeyTable) GetCurrentKey(url string) *SigningKey {
	kt.mu.Lock()
	defer kt.mu.Unlock()

	keys := kt.channels[url]
	if keys == nil {
		return nil
	}
	for _, k := range keys {
		if k.IsCurrent && k.IsActive && k.Status == "active" {
			return k
		}
	}
	return nil
}

// GetActiveKeys returns all active (non-retired, non-revoked) signing keys for a channel.
func (kt *KeyTable) GetActiveKeys(url string) []*SigningKey {
	kt.mu.Lock()
	defer kt.mu.Unlock()

	keys := kt.channels[url]
	if keys == nil {
		return nil
	}

	var active []*SigningKey
	for _, k := range keys {
		if k.IsActive && k.Status == "active" && !k.IsExpired {
			active = append(active, k)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].CreatedAt.Before(active[j].CreatedAt)
	})
	return active
}

// GetKey returns the signing key with the given ID for a channel, or nil if not found.
func (kt *KeyTable) GetKey(url, keyID string) *SigningKey {
	kt.mu.Lock()
	defer kt.mu.Unlock()

	keys := kt.channels[url]
	if keys == nil {
		return nil
	}
	return keys[keyID]
}

// RotateKey creates and activates a new signing key while keeping the existing key
// active for the overlap period.
func (kt *KeyTable) RotateKey(url string, newKeyBytes []byte) (*SigningKey, error) {
	return kt.AddKey(url, newKeyBytes)
}

// RetireKey marks an active, non-current signing key as retired.
func (kt *KeyTable) RetireKey(url, keyID string) error {
	kt.mu.Lock()
	defer kt.mu.Unlock()

	keys := kt.channels[url]
	if keys == nil {
		return fmt.Errorf("no keys configured for URL %s", url)
	}
	k, ok := keys[keyID]
	if !ok {
		return fmt.Errorf("key %s not found for URL %s", keyID, url)
	}
	if k.IsCurrent {
		return fmt.Errorf("cannot retire active current signing key %q; rotate first", keyID)
	}
	k.IsActive = false
	k.IsExpired = true
	k.Status = "retired"
	return nil
}

// RevokeKey immediately invalidates a signing key (emergency path).
func (kt *KeyTable) RevokeKey(url, keyID string) error {
	kt.mu.Lock()
	defer kt.mu.Unlock()

	keys := kt.channels[url]
	if keys == nil {
		return fmt.Errorf("no keys configured for URL %s", url)
	}
	k, ok := keys[keyID]
	if !ok {
		return fmt.Errorf("key %s not found for URL %s", keyID, url)
	}
	k.IsActive = false
	k.IsExpired = true
	k.IsCurrent = false
	k.Status = "revoked"
	return nil
}

// VerifySignature checks whether a delivery signature is valid under any active key for the channel.
func (kt *KeyTable) VerifySignature(url, timestamp string, rawBody []byte, signature, previousSignature string) (string, bool) {
	kt.mu.Lock()
	defer kt.mu.Unlock()

	keys := kt.channels[url]
	if keys == nil {
		return "", false
	}

	keyMap := make(map[string][]byte, len(keys))
	for id, k := range keys {
		if k.IsActive && k.Status == "active" {
			keyMap[id] = k.Key
		}
	}

	// First try primary signature
	if id, ok := VerifySignature(keyMap, VerifyOptions{
		Timestamp: timestamp,
		Body:      rawBody,
		Signature: signature,
	}); ok {
		return id, true
	}

	// Then try previous signature header if provided
	if previousSignature != "" {
		if id, ok := VerifySignature(keyMap, VerifyOptions{
			Timestamp: timestamp,
			Body:      rawBody,
			Signature: previousSignature,
		}); ok {
			return id, true
		}
	}

	return "", false
}
