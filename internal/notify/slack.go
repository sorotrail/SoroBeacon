package notify

import (
	"context"
	"encoding/json"
	"fmt"
)

// slackConfig: {"webhook_url": "https://hooks.slack.com/services/..."}
type slackConfig struct {
	WebhookURL string `json:"webhook_url"`
}

// Slack posts alerts to a Slack incoming webhook.
type Slack struct {
	cfg slackConfig
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
	return &Slack{cfg: cfg}, nil
}

func (s *Slack) Send(ctx context.Context, a Alert) error {
	msg, err := RenderText(a)
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
