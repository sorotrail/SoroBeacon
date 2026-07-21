package notify

import (
	"context"
	"encoding/json"
	"fmt"
)

// discordConfig: {"webhook_url": "https://discord.com/api/webhooks/..."}
type discordConfig struct {
	WebhookURL string `json:"webhook_url"`
}

// Discord posts alerts to a Discord webhook.
type Discord struct {
	cfg discordConfig
}

// NewDiscord builds a Discord notifier from channel config.
func NewDiscord(config json.RawMessage) (Notifier, error) {
	var cfg discordConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("discord: invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("discord: webhook_url is required")
	}
	return &Discord{cfg: cfg}, nil
}

func (d *Discord) Send(ctx context.Context, a Alert) error {
	msg, err := RenderText(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"content": msg})
	if err != nil {
		return err
	}
	if err := postJSON(ctx, d.cfg.WebhookURL, body, nil); err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	return nil
}
