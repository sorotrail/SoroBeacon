package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// mattermostConfig:
// {"webhook_url": "https://mattermost.example.com/hooks/xxx", "channel": "#alerts", "username": "sorobeacon"}
type mattermostConfig struct {
	WebhookURL string `json:"webhook_url"`
	// Channel and Username override the webhook's defaults. Both are
	// optional; when unset they are left out of the payload entirely,
	// because Mattermost treats an empty string as an override too.
	Channel  string `json:"channel,omitempty"`
	Username string `json:"username,omitempty"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// mattermostPayload is the Slack-compatible incoming-webhook body.
type mattermostPayload struct {
	Text        string           `json:"text"`
	Channel     string           `json:"channel,omitempty"`
	Username    string           `json:"username,omitempty"`
	Attachments []chatAttachment `json:"attachments,omitempty"`
}

// Mattermost posts alerts to a Mattermost incoming webhook.
type Mattermost struct {
	cfg mattermostConfig
	tpl channelTemplate
}

// NewMattermost builds a Mattermost notifier from channel config. The
// webhook URL must be HTTPS: it is a bearer credential and would otherwise
// cross the network in the clear.
func NewMattermost(config json.RawMessage) (Notifier, error) {
	var cfg mattermostConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("mattermost: invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("mattermost: webhook_url is required")
	}
	if !strings.HasPrefix(cfg.WebhookURL, "https://") {
		return nil, fmt.Errorf("mattermost: webhook_url must use HTTPS")
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("mattermost: %w", err)
	}
	return &Mattermost{cfg: cfg, tpl: tpl}, nil
}

func (m *Mattermost) Send(ctx context.Context, a Alert) error {
	msg, err := m.tpl.render(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(mattermostPayload{
		Text:        msg,
		Channel:     m.cfg.Channel,
		Username:    m.cfg.Username,
		Attachments: alertAttachments(a, msg),
	})
	if err != nil {
		return err
	}
	if err := postJSON(ctx, m.cfg.WebhookURL, body, nil); err != nil {
		return fmt.Errorf("mattermost: %w", err)
	}
	return nil
}
