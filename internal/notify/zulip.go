package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// zulipConfig:
//
//	{"site": "https://example.zulipchat.com", "bot_email": "beacon-bot@example.zulipchat.com",
//	 "api_key": "...", "stream": "alerts", "topic": "sorobeacon"}
type zulipConfig struct {
	Site     string `json:"site"`
	BotEmail string `json:"bot_email"`
	APIKey   string `json:"api_key"`
	Stream   string `json:"stream"`
	// Topic is optional. When unset each alert goes to a topic named after
	// its monitor, so one noisy monitor does not bury the others.
	Topic string `json:"topic,omitempty"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// zulipMaxTopicLen is Zulip's topic length limit, in characters. The server
// rejects longer topics outright, so a long monitor name is shortened rather
// than failing every delivery.
const zulipMaxTopicLen = 60

// zulipFallbackTopic is used when there is neither a configured topic nor a
// monitor name to derive one from, as with a digest.
const zulipFallbackTopic = "SoroBeacon"

// Zulip posts alerts to a Zulip stream and topic as a bot user.
type Zulip struct {
	cfg      zulipConfig
	endpoint string
	auth     string
	tpl      channelTemplate
}

// NewZulip builds a Zulip notifier from channel config. site must be HTTPS
// because the bot's API key travels in the Authorization header of every
// request.
func NewZulip(config json.RawMessage) (Notifier, error) {
	var cfg zulipConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("zulip: invalid config: %w", err)
	}
	var missing []string
	for _, f := range []struct{ key, val string }{
		{"site", cfg.Site},
		{"bot_email", cfg.BotEmail},
		{"api_key", cfg.APIKey},
		{"stream", cfg.Stream},
	} {
		if strings.TrimSpace(f.val) == "" {
			missing = append(missing, f.key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("zulip: %s is required", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(cfg.Site, "https://") {
		return nil, fmt.Errorf("zulip: site must use HTTPS")
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("zulip: %w", err)
	}
	creds := base64.StdEncoding.EncodeToString([]byte(cfg.BotEmail + ":" + cfg.APIKey))
	return &Zulip{
		cfg:      cfg,
		endpoint: strings.TrimRight(cfg.Site, "/") + "/api/v1/messages",
		auth:     "Basic " + creds,
		tpl:      tpl,
	}, nil
}

func (z *Zulip) Send(ctx context.Context, a Alert) error {
	msg, err := z.tpl.render(a)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("type", "stream")
	form.Set("to", z.cfg.Stream)
	form.Set("topic", z.topic(a))
	form.Set("content", msg)
	if err := postForm(ctx, z.endpoint, form, map[string]string{"Authorization": z.auth}); err != nil {
		return fmt.Errorf("zulip: %w", err)
	}
	return nil
}

// topic picks the configured topic, else the monitor name, else a fixed
// fallback, trimmed to Zulip's limit.
func (z *Zulip) topic(a Alert) string {
	t := z.cfg.Topic
	if strings.TrimSpace(t) == "" {
		t = a.MonitorName
	}
	if strings.TrimSpace(t) == "" {
		t = zulipFallbackTopic
	}
	return truncateRunes(t, zulipMaxTopicLen)
}
