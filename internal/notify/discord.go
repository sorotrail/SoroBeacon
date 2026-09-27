package notify

import (
	"context"
	"encoding/json"
	"fmt"
)

// discordConfig: {"webhook_url": "https://discord.com/api/webhooks/..."}
type discordConfig struct {
	WebhookURL string `json:"webhook_url"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// Discord posts alerts to a Discord webhook.
type Discord struct {
	cfg discordConfig
	tpl channelTemplate
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
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	return &Discord{cfg: cfg, tpl: tpl}, nil
}

func (d *Discord) Send(ctx context.Context, a Alert) error {
	msg, err := d.tpl.render(a)
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
