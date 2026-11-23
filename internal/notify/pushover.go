package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// pushoverConfig: {"token": "<application token>", "user": "<user or group key>", "priority": 0, "device": "phone"}
type pushoverConfig struct {
	Token string `json:"token"`
	User  string `json:"user"`
	// Priority is a pointer so an omitted key (default 0) can be told apart
	// from an explicit value during validation.
	Priority *int   `json:"priority,omitempty"`
	Device   string `json:"device,omitempty"`
	// APIBase overrides https://api.pushover.net, mainly for tests.
	APIBase string `json:"api_base,omitempty"`
	// Template optionally overrides the message body; empty uses the shared
	// default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// Pushover's documented limits, in characters. Oversized fields are rejected
// by the API, so they are truncated here instead: a shortened push is far
// more useful than a failed one.
const (
	pushoverMaxTitle   = 250
	pushoverMaxMessage = 1024
)

// Pushover priorities accepted by this channel. Priority 2 (emergency)
// needs retry/expire parameters and receipt handling, which this channel
// does not implement, so it is rejected rather than half-supported.
const (
	pushoverMinPriority = -2
	pushoverMaxPriority = 1
)

// Pushover sends alerts as push notifications via the Pushover messages API.
type Pushover struct {
	cfg      pushoverConfig
	priority int
	endpoint string
	tpl      channelTemplate
}

// NewPushover builds a Pushover notifier from channel config.
func NewPushover(config json.RawMessage) (Notifier, error) {
	var cfg pushoverConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("pushover: invalid config: %w", err)
	}
	var missing []string
	if strings.TrimSpace(cfg.Token) == "" {
		missing = append(missing, "token")
	}
	if strings.TrimSpace(cfg.User) == "" {
		missing = append(missing, "user")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("pushover: %s is required", strings.Join(missing, ", "))
	}
	priority := 0
	if cfg.Priority != nil {
		priority = *cfg.Priority
	}
	if priority < pushoverMinPriority || priority > pushoverMaxPriority {
		return nil, fmt.Errorf("pushover: priority must be between %d and %d, got %d",
			pushoverMinPriority, pushoverMaxPriority, priority)
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = "https://api.pushover.net"
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("pushover: %w", err)
	}
	return &Pushover{
		cfg:      cfg,
		priority: priority,
		endpoint: strings.TrimRight(apiBase, "/") + "/1/messages.json",
		tpl:      tpl,
	}, nil
}

func (p *Pushover) Send(ctx context.Context, a Alert) error {
	msg, err := p.tpl.render(a)
	if err != nil {
		return err
	}
	title := "SoroBeacon: " + a.MonitorName
	if a.MonitorName == "" {
		title = "SoroBeacon alert"
	}
	form := url.Values{}
	form.Set("token", p.cfg.Token)
	form.Set("user", p.cfg.User)
	form.Set("title", truncateRunes(title, pushoverMaxTitle))
	form.Set("message", truncateRunes(msg, pushoverMaxMessage))
	form.Set("priority", strconv.Itoa(p.priority))
	if p.cfg.Device != "" {
		form.Set("device", p.cfg.Device)
	}
	if err := postForm(ctx, p.endpoint, form, nil); err != nil {
		return fmt.Errorf("pushover: %w", err)
	}
	return nil
}
