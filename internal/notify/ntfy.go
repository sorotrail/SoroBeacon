package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const defaultNtfyServer = "https://ntfy.sh"

// ntfyConfig: {"server_url": "https://ntfy.sh", "topic": "ops", "access_token": "...", "priority": 4}
type ntfyConfig struct {
	ServerURL   string `json:"server_url"`
	Topic       string `json:"topic"`
	AccessToken string `json:"access_token"`
	Priority    *int   `json:"priority"`
}

// Ntfy posts the rendered alert text to an ntfy.sh (or self-hosted) topic.
type Ntfy struct {
	cfg ntfyConfig
}

// NewNtfy builds an ntfy notifier from channel config.
func NewNtfy(config json.RawMessage) (Notifier, error) {
	var cfg ntfyConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("ntfy: invalid config: %w", err)
	}
	cfg.Topic = strings.TrimSpace(cfg.Topic)
	if cfg.Topic == "" {
		return nil, fmt.Errorf("ntfy: topic is required")
	}
	if strings.Contains(cfg.Topic, "/") {
		return nil, fmt.Errorf("ntfy: topic must not contain '/'")
	}
	cfg.ServerURL = strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	if cfg.ServerURL == "" {
		cfg.ServerURL = defaultNtfyServer
	}
	u, err := url.Parse(cfg.ServerURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("ntfy: server_url must be an http or https URL")
	}
	if cfg.Priority != nil {
		p := *cfg.Priority
		if p < 1 || p > 5 {
			return nil, fmt.Errorf("ntfy: priority must be between 1 and 5")
		}
	}
	return &Ntfy{cfg: cfg}, nil
}

func (n *Ntfy) endpoint() string {
	return n.cfg.ServerURL + "/" + url.PathEscape(n.cfg.Topic)
}

func (n *Ntfy) Send(ctx context.Context, a Alert) error {
	msg, err := RenderText(a)
	if err != nil {
		return err
	}
	headers := map[string]string{
		"Title": a.MonitorName,
	}
	if n.cfg.AccessToken != "" {
		headers["Authorization"] = "Bearer " + n.cfg.AccessToken
	}
	if n.cfg.Priority != nil {
		headers["Priority"] = strconv.Itoa(*n.cfg.Priority)
	}
	if err := postPlain(ctx, n.endpoint(), msg, headers); err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	return nil
}
