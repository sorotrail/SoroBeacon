package notify

import (
	"context"
	"encoding/json"
	"fmt"
)

// telegramConfig: {"bot_token": "123:abc", "chat_id": "-100123456"}
type telegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
	// APIBase overrides https://api.telegram.org, mainly for tests.
	APIBase string `json:"api_base,omitempty"`
}

// Telegram sends alerts via the Telegram Bot API (sendMessage).
type Telegram struct {
	cfg telegramConfig
}

// NewTelegram builds a Telegram notifier from channel config.
func NewTelegram(config json.RawMessage) (Notifier, error) {
	var cfg telegramConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("telegram: invalid config: %w", err)
	}
	if cfg.BotToken == "" || cfg.ChatID == "" {
		return nil, fmt.Errorf("telegram: bot_token and chat_id are required")
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.telegram.org"
	}
	return &Telegram{cfg: cfg}, nil
}

func (t *Telegram) Send(ctx context.Context, a Alert) error {
	msg, err := RenderText(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"chat_id": t.cfg.ChatID,
		"text":    msg,
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", t.cfg.APIBase, t.cfg.BotToken)
	if err := postJSON(ctx, url, body, nil); err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	return nil
}
