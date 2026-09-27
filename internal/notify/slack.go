package notify

import (
	"context"
	"encoding/json"
	"fmt"
)

// slackConfig: {"webhook_url": "https://hooks.slack.com/services/..."}
type slackConfig struct {
	WebhookURL string `json:"webhook_url"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// Slack posts alerts to a Slack incoming webhook.
type Slack struct {
	cfg slackConfig
	tpl channelTemplate
}

// NewSlack builds a Slack notifier from channel config.
func NewSlack(config json.RawMessage) (Notifier, error) {
	var cfg slackConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("slack: invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("slack: webhook_url is required")
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("slack: %w", err)
	}
	return &Slack{cfg: cfg, tpl: tpl}, nil
}

func (s *Slack) Send(ctx context.Context, a Alert) error {
	msg, err := s.tpl.render(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"text": msg})
	if err != nil {
		return err
	}
	if err := postJSON(ctx, s.cfg.WebhookURL, body, nil); err != nil {
		return fmt.Errorf("slack: %w", err)
	}
	return nil
}
