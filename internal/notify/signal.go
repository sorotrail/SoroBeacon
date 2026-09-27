package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// signalConfig holds the configuration for the Signal notification channel.
type signalConfig struct {
	APIURL     string   `json:"api_url"`
	Number     string   `json:"number"`
	Recipients []string `json:"recipients"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// Signal posts alerts to a signal-cli-rest-api instance.
type Signal struct {
	cfg signalConfig
	tpl channelTemplate
}

// NewSignal builds a Signal notifier from channel config.
func NewSignal(config json.RawMessage) (Notifier, error) {
	var cfg signalConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("signal: invalid config: %w", err)
	}
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("signal: api_url is required")
	}
	if cfg.Number == "" {
		return nil, fmt.Errorf("signal: number is required")
	}
	if len(cfg.Recipients) == 0 {
		return nil, fmt.Errorf("signal: recipients must be a non-empty array")
	}
	// Normalize API URL: ensure it has a path, trim trailing slash for consistent joining.
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("signal: %w", err)
	}
	return &Signal{cfg: cfg, tpl: tpl}, nil
}

func (s *Signal) Send(ctx context.Context, a Alert) error {
	msg, err := s.tpl.render(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"message":   msg,
		"number":    s.cfg.Number,
		"recipients": s.cfg.Recipients,
	})
	if err != nil {
		return err
	}
	url := s.cfg.APIURL + "/v2/send"
	if err := postJSON(ctx, url, body, nil); err != nil {
		return fmt.Errorf("signal: %w", err)
	}
	return nil
}