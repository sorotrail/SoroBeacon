package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ntfyConfig configures an ntfy channel:
//
//	{
//	  "server_url": "https://ntfy.sh",  // optional, default https://ntfy.sh
//	  "topic": "my-alerts",             // required
//	  "access_token": "tk_...",         // optional bearer token (secret)
//	  "priority": 4                     // optional: 1-5 or min|low|default|high|max|urgent
//	}
//
// Unlike the other HTTP channels, the default server is public and the
// topic name is the only routing secret, so a self-hosted instance is
// addressed by plain http:// over a private network just as often as by
// https://. Both schemes are therefore accepted.
type ntfyConfig struct {
	ServerURL   string          `json:"server_url"`
	Topic       string          `json:"topic"`
	AccessToken string          `json:"access_token"`
	Priority    json.RawMessage `json:"priority"`
}

// ntfyPriorities maps every value ntfy accepts for the Priority header
// (numeric levels and their names) to the numeric form we send. Validating
// here keeps an out-of-range level from being silently ignored by the
// server, which would deliver the alert at the default priority.
var ntfyPriorities = map[string]string{
	"1": "1", "2": "2", "3": "3", "4": "4", "5": "5",
	"min": "1", "low": "2", "default": "3", "high": "4", "max": "5", "urgent": "5",
}

// Ntfy posts the rendered alert text to a topic on ntfy.sh or a
// self-hosted ntfy server.
type Ntfy struct {
	cfg      ntfyConfig
	baseURL  string
	priority string
}

// NewNtfy builds an ntfy notifier from channel config.
func NewNtfy(config json.RawMessage) (Notifier, error) {
	var cfg ntfyConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("ntfy: invalid config: %w", err)
	}
	if cfg.Topic == "" {
		return nil, fmt.Errorf("ntfy: topic is required")
	}
	priority, err := parseNtfyPriority(cfg.Priority)
	if err != nil {
		return nil, err
	}
	// Trailing slashes are a common copy-paste artifact in self-hosted
	// URLs; normalise them away so the topic path never becomes "//topic".
	base := strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	if base == "" {
		base = "https://ntfy.sh"
	}
	return &Ntfy{cfg: cfg, baseURL: base, priority: priority}, nil
}

func (n *Ntfy) Send(ctx context.Context, a Alert) error {
	msg, err := RenderText(a)
	if err != nil {
		return err
	}
	headers := map[string]string{
		"Content-Type": "text/plain; charset=utf-8",
		// ntfy shows the Title as the notification's headline, so the
		// monitor name is the most useful thing to put there.
		"Title": a.MonitorName,
	}
	if n.cfg.AccessToken != "" {
		headers["Authorization"] = "Bearer " + n.cfg.AccessToken
	}
	if n.priority != "" {
		headers["Priority"] = n.priority
	}
	endpoint := n.baseURL + "/" + url.PathEscape(n.cfg.Topic)
	// postJSON never puts the URL or headers into its error, so the token
	// stays out of delivery_attempts and logs.
	if err := postJSON(ctx, endpoint, []byte(msg), headers); err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	return nil
}

// parseNtfyPriority reads an optional priority that may be written as a
// JSON number (4) or a string ("4", "high"). It returns the numeric header
// value, or "" when unset.
func parseNtfyPriority(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("ntfy: invalid priority: %w", err)
	}
	var key string
	switch t := v.(type) {
	case string:
		key = strings.ToLower(strings.TrimSpace(t))
	case float64:
		key = strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return "", fmt.Errorf("ntfy: priority must be a number 1-5 or one of min|low|default|high|max|urgent")
	}
	p, ok := ntfyPriorities[key]
	if !ok {
		return "", fmt.Errorf("ntfy: priority %q is out of range (want 1-5 or min|low|default|high|max|urgent)", key)
	}
	return p, nil
}
