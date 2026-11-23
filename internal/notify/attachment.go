package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// chatAttachment is the Slack-style message attachment understood by both
// Mattermost and Rocket.Chat incoming webhooks. The two formats agree on
// every field used here, so one builder serves both channels.
type chatAttachment struct {
	Fallback string      `json:"fallback,omitempty"`
	Color    string      `json:"color,omitempty"`
	Title    string      `json:"title,omitempty"`
	Fields   []chatField `json:"fields,omitempty"`
}

// chatField is one short key/value pair inside a chatAttachment.
type chatField struct {
	Short bool   `json:"short"`
	Title string `json:"title"`
	Value string `json:"value"`
}

// alertAttachments returns the structured-field attachment for an alert, or
// nil for a digest: a digest summarises many alerts, so per-alert fields
// would describe none of them accurately and the text already says it all.
func alertAttachments(a Alert, fallback string) []chatAttachment {
	if a.Digest != "" {
		return nil
	}
	fields := []chatField{
		{Short: true, Title: "Monitor", Value: a.MonitorName},
		{Short: true, Title: "Rule type", Value: a.RuleType},
		{Short: false, Title: "Event ID", Value: a.EventID},
		{Short: false, Title: "Contract ID", Value: a.ContractID},
		{Short: true, Title: "Ledger", Value: fmt.Sprintf("%d", a.Ledger)},
	}
	// Chat clients render an empty field as a dangling label, so drop them.
	kept := fields[:0]
	for _, f := range fields {
		if f.Value != "" {
			kept = append(kept, f)
		}
	}
	return []chatAttachment{{
		Fallback: fallback,
		Color:    severityColor(a.Severity),
		Title:    "SoroBeacon alert: " + a.MonitorName,
		Fields:   kept,
	}}
}

// severityColor maps an alert severity to the attachment side-bar colour. An
// unknown or empty severity is treated as warning, matching Alert.Severity.
func severityColor(severity string) string {
	switch strings.ToLower(severity) {
	case "critical":
		return "#d00000"
	case "info":
		return "#2f80ed"
	default:
		return "#f2a900"
	}
}

// truncateRunes shortens s to at most limit characters, ending with an
// ellipsis when anything was cut. Services such as Pushover count limits in
// characters rather than bytes, and cutting on a byte boundary could split a
// multi-byte character and produce invalid UTF-8.
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}
