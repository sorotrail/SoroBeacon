package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// SignatureHeader carries the hex HMAC-SHA256 of the canonical signing
// string, keyed with the channel's current active secret.
const SignatureHeader = "X-SoroBeacon-Signature"

// SignaturePreviousHeader carries the same signature computed under
// previous_secret or the rotating-out key.
const SignaturePreviousHeader = "X-SoroBeacon-Signature-Previous"

// TimestampHeader carries the Unix-seconds timestamp the signature is computed over.
const TimestampHeader = "X-SoroBeacon-Timestamp"

// KeyIDHeader carries the key identifier used to sign the request, allowing consumers
// to select the correct verification key directly.
const KeyIDHeader = "X-SoroBeacon-Key-ID"

// IdempotencyHeader carries the stable event identifier so receivers can deduplicate.
const IdempotencyHeader = "X-SoroBeacon-Idempotency-Key"

// WebhookSigningKey represents a key configured on the webhook channel.
type WebhookSigningKey struct {
	ID        string    `json:"id"`
	Secret    string    `json:"secret"`
	IsCurrent bool      `json:"is_current"`
	Status    string    `json:"status"` // "active", "retired", "revoked"
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// webhookConfig holds configuration for the generic webhook channel.
// It supports either a single legacy secret or multiple signing keys with rotation.
type webhookConfig struct {
	URL            string              `json:"url"`
	Secret         string              `json:"secret"`
	PreviousSecret string              `json:"previous_secret"`
	KeyID          string              `json:"key_id"`
	Keys           []WebhookSigningKey `json:"keys"`
}

// Webhook POSTs the full alert as JSON to an arbitrary endpoint, signed
// with HMAC-SHA256 so receivers can authenticate the payload.
type Webhook struct {
	cfg webhookConfig
}

// NewWebhook builds a generic webhook notifier from channel config.
func NewWebhook(config json.RawMessage) (Notifier, error) {
	var cfg webhookConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("webhook: invalid config: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("webhook: url is required")
	}
	if cfg.Secret == "" && len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("webhook: secret is required")
	}
	return &Webhook{cfg: cfg}, nil
}

func (w *Webhook) resolveKeys() (currentKey []byte, currentKeyID string, prevKey []byte, prevKeyID string) {
	if len(w.cfg.Keys) > 0 {
		var activeKeys []WebhookSigningKey
		for _, k := range w.cfg.Keys {
			if k.Status == "active" || k.Status == "" {
				activeKeys = append(activeKeys, k)
			}
		}
		for _, k := range activeKeys {
			if k.IsCurrent {
				currentKey = []byte(k.Secret)
				currentKeyID = k.ID
				break
			}
		}
		if len(currentKey) == 0 && len(activeKeys) > 0 {
			last := activeKeys[len(activeKeys)-1]
			currentKey = []byte(last.Secret)
			currentKeyID = last.ID
		}
		for _, k := range activeKeys {
			if k.ID != currentKeyID {
				prevKey = []byte(k.Secret)
				prevKeyID = k.ID
				break
			}
		}
		return
	}

	currentKey = []byte(w.cfg.Secret)
	currentKeyID = w.cfg.KeyID
	if w.cfg.PreviousSecret != "" {
		prevKey = []byte(w.cfg.PreviousSecret)
	}
	return
}

func (w *Webhook) Send(ctx context.Context, a Alert) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	currentKey, currentKeyID, prevKey, prevKeyID := w.resolveKeys()

	headers := map[string]string{
		TimestampHeader: timestamp,
	}

	if currentKeyID != "" {
		headers[KeyIDHeader] = currentKeyID
		if a.EventID != "" {
			headers[IdempotencyHeader] = a.EventID
		}
		headers[SignatureHeader] = SignCanonical(currentKey, currentKeyID, timestamp, a.EventID, body)
	} else {
		// Legacy single secret without key ID
		headers[SignatureHeader] = Sign(currentKey, timestamp, body)
	}

	if len(prevKey) > 0 {
		if prevKeyID != "" {
			headers[SignaturePreviousHeader] = SignCanonical(prevKey, prevKeyID, timestamp, a.EventID, body)
		} else {
			headers[SignaturePreviousHeader] = Sign(prevKey, timestamp, body)
		}
	}

	if err := postJSON(ctx, w.cfg.URL, body, headers); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return nil
}
