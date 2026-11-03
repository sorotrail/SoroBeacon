package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSigning_CanonicalStringCoversKeyIDAndIdempotency(t *testing.T) {
	// When keyID and idempotency are empty, it matches legacy "<timestamp>.<rawBody>"
	legacy := CanonicalString("", "1700000000", "", []byte(`{"hello":"world"}`))
	assert.Equal(t, "1700000000.{\"hello\":\"world\"}", string(legacy))

	// When keyID is present, it is prepended
	withKey := CanonicalString("csk_1", "1700000000", "", []byte(`{"hello":"world"}`))
	assert.Equal(t, "csk_1.1700000000.{\"hello\":\"world\"}", string(withKey))

	// When keyID and idempotency are present, both are included
	full := CanonicalString("csk_1", "1700000000", "event-999", []byte(`{"hello":"world"}`))
	assert.Equal(t, "csk_1.1700000000.event-999.{\"hello\":\"world\"}", string(full))
}

func TestVerifySignature_KeyIDTamperingFails(t *testing.T) {
	key1 := []byte("secret-key-1")
	key2 := []byte("secret-key-2")
	keys := map[string][]byte{
		"csk_1": key1,
		"csk_2": key2,
	}

	body := []byte(`{"alert_id":42}`)
	timestamp := "1700000000"
	idempotency := "evt-123"

	// Sign under csk_2
	sig := SignCanonical(key2, "csk_2", timestamp, idempotency, body)

	// Valid verification
	matched, ok := VerifySignature(keys, VerifyOptions{
		KeyID:          "csk_2",
		Timestamp:      timestamp,
		IdempotencyKey: idempotency,
		Body:           body,
		Signature:      sig,
	})
	require.True(t, ok)
	assert.Equal(t, "csk_2", matched)

	// Attacker tampers with KeyID header (steering to csk_1)
	_, tamperedOK := VerifySignature(keys, VerifyOptions{
		KeyID:          "csk_1",
		Timestamp:      timestamp,
		IdempotencyKey: idempotency,
		Body:           body,
		Signature:      sig,
	})
	assert.False(t, tamperedOK, "tampered key identifier header must fail verification")

	// Attacker tampers with Idempotency header
	_, tamperedIdemOK := VerifySignature(keys, VerifyOptions{
		KeyID:          "csk_2",
		Timestamp:      timestamp,
		IdempotencyKey: "evt-attacker-changed",
		Body:           body,
		Signature:      sig,
	})
	assert.False(t, tamperedIdemOK, "tampered idempotency header must fail verification")
}

func TestVerifySignature_SkewRejection(t *testing.T) {
	key := []byte("secret")
	keys := map[string][]byte{"k1": key}
	body := []byte(`{}`)

	// Expired timestamp (10 minutes in the past)
	oldTS := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	sig := SignCanonical(key, "k1", oldTS, "", body)

	_, ok := VerifySignature(keys, VerifyOptions{
		KeyID:     "k1",
		Timestamp: oldTS,
		Body:      body,
		Signature: sig,
		MaxSkew:   5 * time.Minute,
	})
	assert.False(t, ok, "signatures outside max skew must be rejected")
}

func TestWebhookDelivery_MultiKeyRotationAndOverlap(t *testing.T) {
	var receivedBody []byte
	var receivedSig, receivedPrevSig, receivedKeyID, receivedIdem, receivedTS string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		receivedSig = r.Header.Get(SignatureHeader)
		receivedPrevSig = r.Header.Get(SignaturePreviousHeader)
		receivedKeyID = r.Header.Get(KeyIDHeader)
		receivedIdem = r.Header.Get(IdempotencyHeader)
		receivedTS = r.Header.Get(TimestampHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Channel config holds multiple keys during overlap:
	// key_old is active but non-current
	// key_new is active and current
	cfg := map[string]any{
		"url": srv.URL,
		"keys": []map[string]any{
			{"id": "csk_old", "secret": "secret-old", "is_current": false, "status": "active"},
			{"id": "csk_new", "secret": "secret-new", "is_current": true, "status": "active"},
		},
	}
	cfgJSON, err := json.Marshal(cfg)
	require.NoError(t, err)

	w, err := NewWebhook(cfgJSON)
	require.NoError(t, err)

	alert := Alert{ID: 10, EventID: "evt-unique-42", MonitorName: "Contract Watcher"}
	require.NoError(t, w.Send(context.Background(), alert))

	// Header checks
	assert.Equal(t, "csk_new", receivedKeyID, "current key ID must be carried in X-SoroBeacon-Key-ID")
	assert.Equal(t, "evt-unique-42", receivedIdem, "event ID must be carried in X-SoroBeacon-Idempotency-Key")
	assert.NotEmpty(t, receivedTS)
	assert.NotEmpty(t, receivedSig)
	assert.NotEmpty(t, receivedPrevSig)

	// Primary signature covers key ID and idempotency under secret-new
	expectedPrimary := SignCanonical([]byte("secret-new"), "csk_new", receivedTS, "evt-unique-42", receivedBody)
	assert.Equal(t, expectedPrimary, receivedSig)

	// Previous signature during overlap covers csk_old under secret-old
	expectedPrev := SignCanonical([]byte("secret-old"), "csk_old", receivedTS, "evt-unique-42", receivedBody)
	assert.Equal(t, expectedPrev, receivedPrevSig)

	// Receiver verification using candidate keyring
	receiverKeyring := map[string][]byte{
		"csk_old": []byte("secret-old"),
		"csk_new": []byte("secret-new"),
	}

	matchedKey, ok := VerifySignature(receiverKeyring, VerifyOptions{
		KeyID:          receivedKeyID,
		Timestamp:      receivedTS,
		IdempotencyKey: receivedIdem,
		Body:           receivedBody,
		Signature:      receivedSig,
		MaxSkew:        DefaultMaxSignatureSkew,
	})
	require.True(t, ok)
	assert.Equal(t, "csk_new", matchedKey)
}

func TestKeyTable_LifecycleAndRevocation(t *testing.T) {
	kt := NewKeyTable()
	url := "https://example.com/webhook"

	// 1. Initial key
	k1, err := kt.AddKey(url, []byte("key-secret-1"))
	require.NoError(t, err)
	assert.True(t, k1.IsCurrent)
	assert.Equal(t, "active", k1.Status)

	curr := kt.GetCurrentKey(url)
	require.NotNil(t, curr)
	assert.Equal(t, k1.ID, curr.ID)

	// 2. Rotate to key 2
	k2, err := kt.RotateKey(url, []byte("key-secret-2"))
	require.NoError(t, err)
	assert.True(t, k2.IsCurrent)
	assert.False(t, k1.IsCurrent, "previous key must no longer be current")

	active := kt.GetActiveKeys(url)
	assert.Len(t, active, 2, "both keys active during overlap")

	// 3. Cannot retire the current key
	err = kt.RetireKey(url, k2.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot retire active current signing key")

	// 4. Retire old key
	require.NoError(t, kt.RetireKey(url, k1.ID))
	assert.Equal(t, "retired", k1.Status)
	assert.False(t, k1.IsActive)

	activeAfterRetire := kt.GetActiveKeys(url)
	assert.Len(t, activeAfterRetire, 1)
	assert.Equal(t, k2.ID, activeAfterRetire[0].ID)

	// 5. Emergency revocation of compromised key
	require.NoError(t, kt.RevokeKey(url, k2.ID))
	assert.Equal(t, "revoked", k2.Status)
	assert.False(t, k2.IsActive)
	assert.False(t, k2.IsCurrent)

	assert.Empty(t, kt.GetActiveKeys(url))
	assert.Nil(t, kt.GetCurrentKey(url))
}
