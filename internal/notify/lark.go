package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// larkConfig holds the configuration for the Lark/Feishu notification channel.
type larkConfig struct {
	WebhookURL string `json:"webhook_url"`
	// Secret is the signing secret for the custom bot. When set, the request
	// body will include timestamp and sign fields for signature verification.
	Secret string `json:"secret,omitempty"`
	// APIBase overrides the webhook URL, mainly for tests.
	APIBase string `json:"api_base,omitempty"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// Lark posts alerts to a Lark/Feishu custom bot webhook.
type Lark struct {
	cfg larkConfig
	tpl channelTemplate
}

// NewLark builds a Lark notifier from channel config.
func NewLark(config json.RawMessage) (Notifier, error) {
	var cfg larkConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("lark: invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("lark: webhook_url is required")
	}
	if cfg.APIBase == "" && !strings.HasPrefix(cfg.WebhookURL, "https://") {
		return nil, fmt.Errorf("lark: webhook_url must use HTTPS")
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("lark: %w", err)
	}
	return &Lark{cfg: cfg, tpl: tpl}, nil
}

func (l *Lark) Send(ctx context.Context, a Alert) error {
	msg, err := l.tpl.render(a)
	if err != nil {
		return err
	}

	// Lark expects a "post" (rich text) or "text" message type.
	// We use "text" for simplicity, which renders as plain text.
	content := map[string]string{"text": msg}
	body := map[string]any{
		"msg_type": "text",
		"content":  content,
	}

	if l.cfg.Secret != "" {
		// Lark signature: HMAC-SHA256 with key = timestamp + "\n" + secret,
		// signing an empty string, then base64 encoded.
		timestamp := time.Now().Unix()
		sign := larkSign(l.cfg.Secret, timestamp)
		body["timestamp"] = timestamp
		body["sign"] = sign
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	url := l.cfg.WebhookURL
	if l.cfg.APIBase != "" {
		url = l.cfg.APIBase
	}

	// Lark returns HTTP 200 with a non-zero code in the response body on failure.
	// We need to inspect the response body to detect errors.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("lark: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lark: post: %w", redactURLError(err))
	}
	defer res.Body.Close()

	resBody, err := io.ReadAll(io.LimitReader(res.Body, 1024))
	if err != nil {
		return fmt.Errorf("lark: read response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("lark: status %d: %s", res.StatusCode, string(resBody))
	}

	// Check for Lark-specific error codes in the response body
	var larkResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(resBody, &larkResp); err == nil && larkResp.Code != 0 {
		return fmt.Errorf("lark: code %d: %s", larkResp.Code, larkResp.Msg)
	}

	return nil
}

// larkSign computes the Lark/Feishu custom bot signature.
// The signing key is timestamp + "\n" + secret, and the message is empty.
// The result is base64 encoded.
func larkSign(secret string, timestamp int64) string {
	stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
	mac := hmac.New(sha256.New, []byte(stringToSign))
	mac.Write([]byte(""))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}