package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// twilioConfig holds configuration for the Twilio SMS notification channel.
type twilioConfig struct {
	AccountSID string   `json:"account_sid"`
	AuthToken  string   `json:"auth_token"`
	From       string   `json:"from"`
	To         []string `json:"to"`
	APIBase    string   `json:"api_base,omitempty"`
	Template   string   `json:"template,omitempty"`
}

// Twilio sends SMS alerts via the Twilio Messages API.
type Twilio struct {
	cfg      twilioConfig
	formBase string
	tpl      channelTemplate
}

// NewTwilio builds a Twilio notifier from channel config.
func NewTwilio(config json.RawMessage) (Notifier, error) {
	var cfg twilioConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("twilio: invalid config: %w", err)
	}
	if cfg.AccountSID == "" || cfg.AuthToken == "" || cfg.From == "" || len(cfg.To) == 0 {
		return nil, fmt.Errorf("twilio: account_sid, auth_token, from and a non-empty to are required")
	}
	for _, recipient := range cfg.To {
		if strings.TrimSpace(recipient) == "" {
			return nil, fmt.Errorf("twilio: recipient in to cannot be empty")
		}
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = "https://api.twilio.com"
	}
	formBase := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", apiBase, cfg.AccountSID)
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("twilio: %w", err)
	}
	return &Twilio{
		cfg:      cfg,
		formBase: formBase,
		tpl:      tpl,
	}, nil
}

// Send sends the alert via SMS to each recipient in the to list.
func (t *Twilio) Send(ctx context.Context, a Alert) error {
	msg, err := t.tpl.render(a)
	if err != nil {
		return err
	}

	// SMS has a hard length limit; compact summary truncated to fit.
	const maxSMSLen = 160
	if len(msg) > maxSMSLen {
		// Produce a compact fallback or truncate safely
		shortContract := a.ContractID
		if len(shortContract) > 12 {
			shortContract = shortContract[:6] + "..." + shortContract[len(shortContract)-6:]
		}
		compact := fmt.Sprintf("SoroBeacon: %s | %s | %s", a.MonitorName, a.EventName, shortContract)
		if len(compact) > maxSMSLen {
			compact = compact[:maxSMSLen]
		}
		msg = compact
	}

	var successCount int
	var lastErr error

	for _, recipient := range t.cfg.To {
		formValues := url.Values{}
		formValues.Set("From", t.cfg.From)
		formValues.Set("To", recipient)
		formValues.Set("Body", msg)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.formBase, strings.NewReader(formValues.Encode()))
		if err != nil {
			lastErr = fmt.Errorf("build request: %w", err)
			continue
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		authCredentials := t.cfg.AccountSID + ":" + t.cfg.AuthToken
		encodedAuth := base64.StdEncoding.EncodeToString([]byte(authCredentials))
		req.Header.Set("Authorization", "Basic "+encodedAuth)

		res, err := httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("post: %w", redactURLError(err))
			continue
		}
		bodyBytes, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		res.Body.Close()

		if res.StatusCode < 200 || res.StatusCode > 299 {
			lastErr = fmt.Errorf("status %d: %s", res.StatusCode, string(bodyBytes))
			continue
		}

		successCount++
	}

	if successCount == 0 && lastErr != nil {
		return fmt.Errorf("twilio: all %d recipients failed, last error: %w", len(t.cfg.To), lastErr)
	}
	if successCount < len(t.cfg.To) {
		return fmt.Errorf("twilio: %d of %d recipients succeeded, last error: %w", successCount, len(t.cfg.To), lastErr)
	}

	return nil
}
