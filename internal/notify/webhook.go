package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// SignatureHeader carries the hex HMAC-SHA256 of the request body, keyed
// with the channel's secret. Receivers should recompute and compare.
const SignatureHeader = "X-SoroBeacon-Signature"

// webhookConfig: {"url": "https://example.com/hook", "secret": "shared-secret"}
type webhookConfig struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
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
	headers := map[string]string{SignatureHeader: Sign(w.cfg.Secret, body)}
	if err := postJSON(ctx, w.cfg.URL, body, headers); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return nil
}

// Sign returns the hex HMAC-SHA256 of body under secret. Exported so
// webhook receivers written in Go can verify payloads with the same code.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
