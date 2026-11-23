package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// rocketChatConfig:
// {"webhook_url": "https://chat.example.com/hooks/xxxx/yyyy", "alias": "SoroBeacon", "emoji": ":satellite:"}
type rocketChatConfig struct {
	WebhookURL string `json:"webhook_url"`
	// Alias and Emoji override the display name and avatar the webhook
	// integration was created with. Both are optional and omitted from the
	// payload when unset.
	Alias string `json:"alias,omitempty"`
	Emoji string `json:"emoji,omitempty"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// rocketChatPayload is the Rocket.Chat incoming-webhook body.
type rocketChatPayload struct {
	Text        string           `json:"text"`
	Alias       string           `json:"alias,omitempty"`
	Emoji       string           `json:"emoji,omitempty"`
	Attachments []chatAttachment `json:"attachments,omitempty"`
}

// RocketChat posts alerts to a Rocket.Chat incoming webhook.
type RocketChat struct {
	cfg rocketChatConfig
	tpl channelTemplate
}

// NewRocketChat builds a Rocket.Chat notifier from channel config. The
// webhook URL embeds its token in the path, so it must be HTTPS.
func NewRocketChat(config json.RawMessage) (Notifier, error) {
	var cfg rocketChatConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("rocketchat: invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("rocketchat: webhook_url is required")
	}
	if !strings.HasPrefix(cfg.WebhookURL, "https://") {
		return nil, fmt.Errorf("rocketchat: webhook_url must use HTTPS")
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("rocketchat: %w", err)
	}
	return &RocketChat{cfg: cfg, tpl: tpl}, nil
}

func (r *RocketChat) Send(ctx context.Context, a Alert) error {
	msg, err := r.tpl.render(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(rocketChatPayload{
		Text:        msg,
		Alias:       r.cfg.Alias,
		Emoji:       r.cfg.Emoji,
		Attachments: alertAttachments(a, msg),
	})
	if err != nil {
		return err
	}
	if err := postJSON(ctx, r.cfg.WebhookURL, body, nil); err != nil {
		// Report the status code alone. Rocket.Chat integration scripts can
		// shape the error body, and a self-hosted script may echo the
		// request (and with it the hook path) back.
		var se *StatusError
		if errors.As(err, &se) {
			return fmt.Errorf("rocketchat: status %d", se.Code)
		}
		return fmt.Errorf("rocketchat: %w", err)
	}
	return nil
}
