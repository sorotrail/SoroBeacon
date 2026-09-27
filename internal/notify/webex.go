package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// webexConfig holds the configuration for the Webex notification channel.
type webexConfig struct {
	BotToken string `json:"bot_token"`
	RoomID   string `json:"room_id"`
	// APIBase overrides https://webexapis.com, mainly for tests.
	APIBase  string `json:"api_base,omitempty"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// Webex posts alerts to a Webex room using a bot access token.
type Webex struct {
	cfg webexConfig
	tpl channelTemplate
}

// NewWebex builds a Webex notifier from channel config.
func NewWebex(config json.RawMessage) (Notifier, error) {
	var cfg webexConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("webex: invalid config: %w", err)
	}
	if cfg.BotToken == "" || cfg.RoomID == "" {
		return nil, fmt.Errorf("webex: bot_token and room_id are required")
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://webexapis.com"
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("webex: %w", err)
	}
	return &Webex{cfg: cfg, tpl: tpl}, nil
}

func (w *Webex) Send(ctx context.Context, a Alert) error {
	msg, err := w.tpl.render(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"roomId":  w.cfg.RoomID,
		"markdown": msg,
	})
	if err != nil {
		return err
	}
	url := w.cfg.APIBase + "/v1/messages"
	headers := map[string]string{
		"Authorization": "Bearer " + w.cfg.BotToken,
	}
	if err := requestJSON(ctx, "POST", url, body, headers); err != nil {
		// Provide a more helpful error for 401/403 without leaking the token
		if strings.Contains(err.Error(), "status 401") || strings.Contains(err.Error(), "status 403") {
			return fmt.Errorf("webex: check the bot token and that the bot is a member of the room")
		}
		return fmt.Errorf("webex: %w", err)
	}
	return nil
}