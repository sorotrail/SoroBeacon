// Package notify delivers alerts to notification channels.
//
// Contributors: add a new channel type by implementing Notifier and
// registering a constructor in DefaultFactory (or on your own Factory).
// Channel config arrives as the channel's raw JSON config; never log it —
// it contains secrets.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/sorotrail/sorobeacon/internal/secrets"
)

// Alert is the rendered-alert payload handed to a Notifier. It is a
// flattened, channel-agnostic view of a stored alert plus its context.
type Alert struct {
	ID          int64           `json:"id"`
	MonitorID   int64           `json:"monitor_id"`
	MonitorName string          `json:"monitor_name"`
	RuleID      int64           `json:"rule_id"`
	RuleType    string          `json:"rule_type"`
	EventID     string          `json:"event_id"`
	ContractID  string          `json:"contract_id"`
	EventName   string          `json:"event_name"`
	Ledger      uint32          `json:"ledger"`
	TxHash      string          `json:"tx_hash"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	// Severity is the alert severity (info, warning, critical). Empty means
	// warning for backwards compatibility.
	Severity string `json:"severity,omitempty"`
	// Digest carries a pre-rendered summary when this Alert represents a
	// channel digest rather than a single event. RenderText returns it
	// verbatim, so every text channel sends the same summary without
	// needing a digest-specific method.
	Digest string `json:"digest,omitempty"`
}

// Notifier sends one alert to one destination. Implementations should
// return an error whose message is safe to persist as a delivery
// response_snippet (no secrets).
type Notifier interface {
	Send(ctx context.Context, a Alert) error
}

// Constructor builds a Notifier from a channel's JSON config.
type Constructor func(config json.RawMessage) (Notifier, error)

// Factory builds Notifiers by channel type name. When a secrets resolver is
// attached, every constructor resolves ${secret:...} references in the
// config before it runs, so all channel types support external secrets
// without individual changes.
type Factory struct {
	constructors map[string]Constructor
	secrets      *secrets.Resolver
}

// Channel type names understood by DefaultFactory.
const (
	TypeDiscord   = "discord"
	TypeSlack     = "slack"
	TypeTelegram  = "telegram"
	TypeEmail     = "email"
	TypeWebhook   = "webhook"
	TypeMatrix    = "matrix"
	TypePagerDuty = "pagerduty"
	TypeTwilio    = "twilio"
	TypeSignal    = "signal"
	TypeWebex     = "webex"
	TypeDingTalk  = "dingtalk"
)

// DefaultFactory returns a Factory with the built-in channel types.
func DefaultFactory() *Factory {
	f := &Factory{constructors: map[string]Constructor{}}
	f.Register(TypeDiscord, NewDiscord)
	f.Register(TypeSlack, NewSlack)
	f.Register(TypeTelegram, NewTelegram)
	f.Register(TypeEmail, NewEmail)
	f.Register(TypeWebhook, NewWebhook)
	f.Register(TypeMatrix, NewMatrix)
	f.Register(TypePagerDuty, NewPagerDuty)
	f.Register(TypeFederation, NewFederation)
	f.Register(TypeTwilio, NewTwilio)
	f.Register(TypeSignal, NewSignal)
	f.Register(TypeWebex, NewWebex)
	f.Register(TypeDingTalk, NewDingTalk)
	return f
}

// Register adds (or replaces) a channel type.
func (f *Factory) Register(name string, c Constructor) {
	f.constructors[name] = c
}

// Types returns the registered channel type names.
func (f *Factory) Types() []string {
	out := make([]string, 0, len(f.constructors))
	for name := range f.constructors {
		out = append(out, name)
	}
	return out
}

// WithSecrets attaches an external-secret resolver. It is optional: without
// one, config values (including anything that looks like a reference) are
// passed through as literals, which is exactly the behaviour before external
// secrets existed.
func (f *Factory) WithSecrets(r *secrets.Resolver) *Factory {
	f.secrets = r
	return f
}

// InvalidateSecretCache drops cached secret values. The API calls it when a
// channel is created, updated or deleted so a corrected or rotated secret
// takes effect on the next delivery instead of waiting for the TTL.
func (f *Factory) InvalidateSecretCache() {
	if f.secrets != nil {
		f.secrets.Invalidate()
	}
}

// New builds a Notifier for a channel type and config. References are
// resolved against the attached resolver, if any; the resolved copy stays in
// memory with the notifier and is never written back, logged or returned.
func (f *Factory) New(channelType string, config json.RawMessage) (Notifier, error) {
	c, ok := f.constructors[channelType]
	if !ok {
		return nil, fmt.Errorf("unknown channel type %q (registered: %v)", channelType, f.Types())
	}
	resolved := config
	if f.secrets != nil && len(config) > 0 {
		r, err := f.secrets.ResolveConfig(context.Background(), config)
		if err != nil {
			return nil, err
		}
		resolved = r
	}
	return c(resolved)
}

// defaultTemplate renders the plain-text alert message shared by the chat
// and email channels. The generic webhook sends structured JSON instead.
var defaultTemplate = template.Must(template.New("alert").Parse(strings.TrimSpace(`
🔔 SoroBeacon alert: {{.MonitorName}}
Rule: {{.RuleType}} (#{{.RuleID}})
Contract: {{.ContractID}}
{{- if .EventName}}
Event: {{.EventName}}{{end}}
Ledger: {{.Ledger}}
Tx: {{.TxHash}}
Event ID: {{.EventID}}
At: {{.CreatedAt.UTC.Format "2006-01-02 15:04:05"}} UTC
{{- if gt .GroupCount 0}}
Group: {{.GroupCount}} alert(s) in window {{.WindowStart.UTC.Format "2006-01-02T15:04:05Z"}} to {{.WindowEnd.UTC.Format "2006-01-02T15:04:05Z"}}
{{- end}}
`)))

// RenderText renders the default plain-text message for an alert. A digest
// Alert (one with Digest set) returns the summary unchanged: it is already a
// finished message.
func RenderText(a Alert) (string, error) {
	if a.Digest != "" {
		return a.Digest, nil
	}
	var b strings.Builder
	if err := defaultTemplate.Execute(&b, a); err != nil {
		return "", fmt.Errorf("render alert message: %w", err)
	}
	return b.String(), nil
}

// DefaultDigestMaxLen bounds a digest message in bytes. It is documented in
// docs/channels/digest.md; a longer digest ends with an explicit "and N more"
// line rather than being cut off mid-line.
const DefaultDigestMaxLen = 2000

// RenderDigest builds the summary message a digest channel sends after its
// window elapses. It states how many alerts it covers, groups them by
// monitor, and truncates to maxLen (DefaultDigestMaxLen when maxLen <= 0)
// with an explicit "and N more" line. An empty slice renders "" — the caller
// sends nothing.
func RenderDigest(alerts []Alert, maxLen int) string {
	if len(alerts) == 0 {
		return ""
	}
	if maxLen <= 0 {
		maxLen = DefaultDigestMaxLen
	}
	type group struct {
		name   string
		alerts []Alert
	}
	var groups []group
	index := map[string]int{}
	for _, a := range alerts {
		name := a.MonitorName
		if name == "" {
			name = "(unknown monitor)"
		}
		i, ok := index[name]
		if !ok {
			i = len(groups)
			index[name] = i
			groups = append(groups, group{name: name})
		}
		groups[i].alerts = append(groups[i].alerts, a)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SoroBeacon digest: %d alert(s) across %d monitor(s)\n", len(alerts), len(groups))
	omitted := 0
	for _, g := range groups {
		header := fmt.Sprintf("\n%s (%d)\n", g.name, len(g.alerts))
		if b.Len()+len(header) > maxLen {
			omitted += len(g.alerts)
			continue
		}
		b.WriteString(header)
		for _, a := range g.alerts {
			line := fmt.Sprintf("- %s\n", digestLine(a))
			if b.Len()+len(line) > maxLen {
				omitted++
				continue
			}
			b.WriteString(line)
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n... and %d more\n", omitted)
	}
	return strings.TrimRight(b.String(), "\n")
}

// digestLine is one alert's one-line summary inside a digest.
func digestLine(a Alert) string {
	parts := []string{a.EventName}
	if a.ContractID != "" {
		parts = append(parts, a.ContractID)
	}
	parts = append(parts, fmt.Sprintf("ledger %d", a.Ledger))
	if !a.CreatedAt.IsZero() {
		parts = append(parts, a.CreatedAt.UTC().Format("2006-01-02 15:04:05")+" UTC")
	}
	return strings.Join(parts, "  ")
}

// channelTemplate is an optional per-channel override of the plain-text
// message. It is parsed once, when the channel's config is constructed, so a
// syntax error is rejected when the channel is created or updated (an HTTP
// 400 naming the parse error) rather than discovered at delivery time.
//
// The data is the exported Alert struct: a template may reference any of its
// fields (see docs/channels/templates.md for the list). text/template is used
// deliberately, not html/template: the destination owns escaping for its own
// rendering context, so field values are inserted verbatim.
//
// The zero value means "no override": render then falls back to RenderText.
type channelTemplate struct {
	tpl *template.Template
}

// parseChannelTemplate compiles a channel's optional template. An empty (or
// whitespace-only) string means no override. The error wraps the parser's
// message so the API can return it to the operator.
func parseChannelTemplate(raw string) (channelTemplate, error) {
	if strings.TrimSpace(raw) == "" {
		return channelTemplate{}, nil
	}
	tpl, err := template.New("alert").Parse(raw)
	if err != nil {
		return channelTemplate{}, fmt.Errorf("invalid template: %w", err)
	}
	return channelTemplate{tpl: tpl}, nil
}

// render executes the override, or the default message when none is set. A
// template that parses but fails at execution — an unknown field, a bad index
// into a slice — falls back to the default and logs a warning: an alert must
// never be dropped over a formatting mistake.
func (t channelTemplate) render(a Alert) (string, error) {
	// A digest summary is already rendered; a per-alert template must not
	// reshape it.
	if a.Digest != "" {
		return a.Digest, nil
	}
	if t.tpl == nil {
		return RenderText(a)
	}
	var b strings.Builder
	if err := t.tpl.Execute(&b, a); err != nil {
		slog.Warn("channel template failed to execute; using the default message", "err", err)
		return RenderText(a)
	}
	return b.String(), nil
}
