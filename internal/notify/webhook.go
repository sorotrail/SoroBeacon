package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// SignatureHeader carries the hex HMAC-SHA256 of the canonical signing
// string (timestamp + "." + raw body), keyed with the channel's current
// secret. Receivers should recompute and compare.
const SignatureHeader = "X-SoroBeacon-Signature"

// SignaturePreviousHeader carries the same signature computed under
// previous_secret. It is only sent while a secret rotation is in flight so
// a receiver that has not switched keys yet can still verify a delivery.
const SignaturePreviousHeader = "X-SoroBeacon-Signature-Previous"

// TimestampHeader carries the Unix-seconds timestamp the signature is
// computed over. It is deliberately part of the signed string so receivers
// can reject replays: a bare signature over the body alone would let anyone
// who captures one request replay it forever.
const TimestampHeader = "X-SoroBeacon-Timestamp"

// webhookConfig: {"url": "https://example.com/hook", "secret": "shared-secret",
// "previous_secret": "the-secret-we-are-rotating-from"}
type webhookConfig struct {
	URL string `json:"url"`
	// Secret signs every request; it is the key a receiver should hold.
	Secret string `json:"secret"`
	// PreviousSecret, when set, is used to emit a second signature so a
	// receiver can roll over without dropping deliveries mid-rotation.
	PreviousSecret string `json:"previous_secret"`
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
	if cfg.Secret == "" {
		return nil, fmt.Errorf("webhook: secret is required")
	}
	return &Webhook{cfg: cfg}, nil
}

func (w *Webhook) Send(ctx context.Context, a Alert) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}

	// The timestamp is computed once and reused for both signatures; a
	// receiver validates each header against the same signed string.
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	headers := map[string]string{
		TimestampHeader: timestamp,
		SignatureHeader: Sign(w.cfg.Secret, timestamp, body),
	}
	if w.cfg.PreviousSecret != "" {
		headers[SignaturePreviousHeader] = Sign(w.cfg.PreviousSecret, timestamp, body)
	}

	if err := postJSON(ctx, w.cfg.URL, body, headers); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return nil
}

// Sign returns the hex HMAC-SHA256 of the canonical signing string under
// secret. The canonical string is the decimal Unix timestamp, a single "."
// and then the raw request body, byte for byte:
//
//	<timestamp>.<body>
//
// Exported so receivers written in Go can verify payloads with the same
// code the sender uses.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
