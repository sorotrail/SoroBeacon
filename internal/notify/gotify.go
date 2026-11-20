package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// gotifyConfig holds the configuration for the Gotify notification channel.
type gotifyConfig struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token"`
	Priority  int    `json:"priority,omitempty"`
}

const defaultGotifyPriority = 5

// Gotify posts alerts to a Gotify server using an application token.
type Gotify struct {
	cfg gotifyConfig
}

// NewGotify builds a Gotify notifier from channel config.
func NewGotify(config json.RawMessage) (Notifier, error) {
	var cfg gotifyConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("gotify: invalid config: %w", err)
	}
	if cfg.ServerURL == "" {
		return nil, fmt.Errorf("gotify: server_url is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("gotify: token is required")
	}
	// Normalize server URL: ensure it has a scheme and trim trailing slash.
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	if !strings.HasPrefix(cfg.ServerURL, "http://") && !strings.HasPrefix(cfg.ServerURL, "https://") {
		return nil, fmt.Errorf("gotify: server_url must use HTTP or HTTPS")
	}
	if cfg.Priority == 0 {
		cfg.Priority = defaultGotifyPriority
	}
	return &Gotify{cfg: cfg}, nil
}

func (g *Gotify) Send(ctx context.Context, a Alert) error {
	msg, err := g.renderMessage(a)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"title":    "SoroBeacon Alert",
		"message":  msg,
		"priority": g.cfg.Priority,
	})
	if err != nil {
		return err
	}

	url := g.cfg.ServerURL + "/message?token=" + g.cfg.Token
	if err := postJSON(ctx, url, body, nil); err != nil {
		return fmt.Errorf("gotify: %w", err)
	}
	return nil
}

func (g *Gotify) renderMessage(a Alert) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Monitor: %s\n", a.MonitorName)
	fmt.Fprintf(&b, "Rule: %s (#%d)\n", a.RuleType, a.RuleID)
	fmt.Fprintf(&b, "Contract: %s\n", a.ContractID)
	if a.EventName != "" {
		fmt.Fprintf(&b, "Event: %s\n", a.EventName)
	}
	fmt.Fprintf(&b, "Ledger: %d\n", a.Ledger)
	fmt.Fprintf(&b, "Tx: %s\n", a.TxHash)
	fmt.Fprintf(&b, "Event ID: %s\n", a.EventID)
	fmt.Fprintf(&b, "At: %s UTC\n", a.CreatedAt.UTC().Format("2006-01-02 15:04:05"))
	return b.String(), nil
}
