package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// teamsConfig: {"webhook_url": "https://<tenant>.webhook.office.com/webhookb2/..."}
type teamsConfig struct {
	WebhookURL string `json:"webhook_url"`
	// Template optionally overrides the plain-text message; empty uses the
	// shared default (see RenderText and docs/channels/templates.md).
	Template string `json:"template,omitempty"`
}

// Teams posts alerts to a Microsoft Teams channel via an Incoming Webhook,
// rendered as a MessageCard so the monitor name, rule type, event ID and
// timestamp are readable at a glance rather than as an unformatted blob.
type Teams struct {
	cfg teamsConfig
	tpl channelTemplate
}

// NewTeams builds a Teams notifier from channel config. The URL is validated
// up front so a bad destination fails at channel creation with a 400 rather
// than at first delivery; the error names the problem but never the URL,
// which is a secret like any other webhook destination.
func NewTeams(config json.RawMessage) (Notifier, error) {
	var cfg teamsConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("teams: invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("teams: webhook_url is required")
	}
	// Incoming webhooks are always HTTPS. Loopback HTTP is accepted only so
	// the httptest servers in this package's tests can stand in for Teams —
	// the same concession googlechat makes, and unreachable from a real
	// deployment.
	if !strings.HasPrefix(cfg.WebhookURL, "https://") &&
		!strings.HasPrefix(cfg.WebhookURL, "http://localhost") &&
		!strings.HasPrefix(cfg.WebhookURL, "http://127.0.0.1") {
		return nil, fmt.Errorf("teams: webhook_url must use HTTPS")
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("teams: %w", err)
	}
	return &Teams{cfg: cfg, tpl: tpl}, nil
}

func (t *Teams) Send(ctx context.Context, a Alert) error {
	msg, err := t.tpl.render(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(buildTeamsMessageCard(a, msg))
	if err != nil {
		return err
	}
	if err := postJSON(ctx, t.cfg.WebhookURL, body, nil); err != nil {
		return fmt.Errorf("teams: %w", err)
	}
	return nil
}

// teamsMessageCard is the MessageCard body Teams renders. It follows the
// classic connector schema (@type/@context), which every Microsoft 365
// incoming webhook accepts.
type teamsMessageCard struct {
	Type    string `json:"@type"`
	Context string `json:"@context"`
	Summary string `json:"summary"`
	Title   string `json:"title"`
	// ThemeColor carries the alert severity as a hex colour so a critical
	// alert is distinguishable from an info one at a glance in the list.
	ThemeColor string            `json:"themeColor"`
	Sections   []teamsCardSection `json:"sections"`
}

type teamsCardSection struct {
	// ActivityTitle carries the monitor name as the card's headline fact.
	Facts []teamsCardFact `json:"facts"`
	Text  string          `json:"text"`
}

type teamsCardFact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// teamsThemeColor maps an alert severity onto the card accent. An empty
// severity means warning, matching the rest of the notification layer.
func teamsThemeColor(severity string) string {
	switch severity {
	case "critical":
		return "FF0000"
	case "info":
		return "0076D7"
	default:
		return "FFA500"
	}
}

// buildTeamsMessageCard renders one alert as a MessageCard. The message text
// (a digest summary when this alert is one) is the card body, and the
// identifying fields become facts so they are scannable without reading
// prose.
func buildTeamsMessageCard(a Alert, msg string) teamsMessageCard {
	facts := []teamsCardFact{
		{Name: "Monitor", Value: a.MonitorName},
		{Name: "Rule", Value: fmt.Sprintf("%s (#%d)", a.RuleType, a.RuleID)},
	}
	if a.EventName != "" {
		facts = append(facts, teamsCardFact{Name: "Event", Value: a.EventName})
	}
	if a.ContractID != "" {
		facts = append(facts, teamsCardFact{Name: "Contract", Value: a.ContractID})
	}
	if a.Ledger > 0 {
		facts = append(facts, teamsCardFact{Name: "Ledger", Value: fmt.Sprintf("%d", a.Ledger)})
	}
	if a.TxHash != "" {
		facts = append(facts, teamsCardFact{Name: "Tx", Value: a.TxHash})
	}
	facts = append(facts,
		teamsCardFact{Name: "Event ID", Value: a.EventID},
		teamsCardFact{Name: "At", Value: a.CreatedAt.UTC().Format("2006-01-02 15:04:05") + " UTC"},
	)

	summary := "SoroBeacon alert"
	if a.MonitorName != "" {
		summary = "SoroBeacon alert: " + a.MonitorName
	}
	return teamsMessageCard{
		Type:       "MessageCard",
		Context:    "http://schema.org/extensions",
		Summary:    summary,
		Title:      summary,
		ThemeColor: teamsThemeColor(a.Severity),
		Sections: []teamsCardSection{
			{Facts: facts, Text: msg},
		},
	}
}
