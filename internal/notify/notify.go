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
	"strings"
	"text/template"
	"time"
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
}

// Notifier sends one alert to one destination. Implementations should
// return an error whose message is safe to persist as a delivery
// response_snippet (no secrets).
type Notifier interface {
	Send(ctx context.Context, a Alert) error
}

// Constructor builds a Notifier from a channel's JSON config.
type Constructor func(config json.RawMessage) (Notifier, error)

// Factory builds Notifiers by channel type name.
type Factory struct {
	constructors map[string]Constructor
}

// Channel type names understood by DefaultFactory.
const (
	TypeDiscord  = "discord"
	TypeSlack    = "slack"
	TypeTelegram = "telegram"
	TypeEmail    = "email"
	TypeWebhook  = "webhook"
)

// DefaultFactory returns a Factory with the built-in channel types.
func DefaultFactory() *Factory {
	f := &Factory{constructors: map[string]Constructor{}}
	f.Register(TypeDiscord, NewDiscord)
	f.Register(TypeSlack, NewSlack)
	f.Register(TypeTelegram, NewTelegram)
	f.Register(TypeEmail, NewEmail)
	f.Register(TypeWebhook, NewWebhook)
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

// New builds a Notifier for a channel type and config.
func (f *Factory) New(channelType string, config json.RawMessage) (Notifier, error) {
	c, ok := f.constructors[channelType]
	if !ok {
		return nil, fmt.Errorf("unknown channel type %q (registered: %v)", channelType, f.Types())
	}
	return c(config)
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
`)))

// RenderText renders the default plain-text message for an alert.
func RenderText(a Alert) (string, error) {
	var b strings.Builder
	if err := defaultTemplate.Execute(&b, a); err != nil {
		return "", fmt.Errorf("render alert message: %w", err)
	}
	return b.String(), nil
}
