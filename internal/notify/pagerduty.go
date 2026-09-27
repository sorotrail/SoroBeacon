package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// pagerDutyConfig: {"routing_key": "R0UT1NGK3Y", "severity": "warning"}
type pagerDutyConfig struct {
	RoutingKey string `json:"routing_key"`
	// Severity is the PagerDuty incident severity; empty defaults to
	// "warning". PagerDuty rejects anything outside the allowlist.
	Severity string `json:"severity,omitempty"`
	// EventsURL overrides the Events API base URL, mainly for tests.
	EventsURL string `json:"events_url,omitempty"`
}

// pagerDutySeverities are the values the Events API v2 accepts.
var pagerDutySeverities = map[string]bool{
	"critical": true,
	"error":    true,
	"warning":  true,
	"info":     true,
}

// defaultPagerDutyEventsURL is the Events API v2 base. It carries no secret,
// so like the routing key it is safe to see in configuration and docs.
const defaultPagerDutyEventsURL = "https://events.pagerduty.com"

// PagerDuty opens incidents through the PagerDuty Events API v2.
//
// The dedup key is derived from (rule_id, event_id), mirroring SoroBeacon's
// own dedup guard: if the same alert is delivered twice, PagerDuty folds it
// into one incident instead of paging a human twice.
type PagerDuty struct {
	cfg pagerDutyConfig
}

// NewPagerDuty builds a PagerDuty notifier from channel config.
func NewPagerDuty(config json.RawMessage) (Notifier, error) {
	var cfg pagerDutyConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("pagerduty: invalid config: %w", err)
	}
	if cfg.RoutingKey == "" {
		return nil, fmt.Errorf("pagerduty: routing_key is required")
	}
	if cfg.Severity == "" {
		cfg.Severity = "warning"
	}
	if !pagerDutySeverities[cfg.Severity] {
		return nil, fmt.Errorf("pagerduty: invalid severity %q (want critical, error, warning or info)", cfg.Severity)
	}
	if cfg.EventsURL == "" {
		cfg.EventsURL = defaultPagerDutyEventsURL
	}
	cfg.EventsURL = strings.TrimRight(cfg.EventsURL, "/")
	return &PagerDuty{cfg: cfg}, nil
}

// dedupKey is deterministic for a given (rule_id, event_id) pair, so a
// redelivered alert does not open a second incident.
func dedupKey(a Alert) string {
	return fmt.Sprintf("%d:%s", a.RuleID, a.EventID)
}

func (p *PagerDuty) Send(ctx context.Context, a Alert) error {
	details := map[string]any{
		"monitor_id":   a.MonitorID,
		"monitor_name": a.MonitorName,
		"rule_id":      a.RuleID,
		"rule_type":    a.RuleType,
		"event_id":     a.EventID,
		"contract_id":  a.ContractID,
		"event_name":   a.EventName,
		"ledger":       a.Ledger,
		"tx_hash":      a.TxHash,
	}
	// Fold the alert payload in so the incident carries the event context.
	// It is the alert payload, never the channel config, so no routing key
	// or other channel secret can reach PagerDuty.
	if len(a.Payload) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(a.Payload, &payload); err == nil {
			for k, v := range payload {
				details[k] = v
			}
		}
	}

	body, err := json.Marshal(map[string]any{
		"routing_key":  p.cfg.RoutingKey,
		"event_action": "trigger",
		"dedup_key":    dedupKey(a),
		"payload": map[string]any{
			"summary":        fmt.Sprintf("SoroBeacon alert: %s", a.MonitorName),
			"source":         a.ContractID,
			"severity":       p.cfg.Severity,
			"custom_details": details,
		},
	})
	if err != nil {
		return err
	}

	if err := postJSON(ctx, p.cfg.EventsURL+"/v2/enqueue", body, nil); err != nil {
		// postJSON's error carries only the status and a response-body
		// snippet, so the routing key stays out of the delivery snippet.
		return fmt.Errorf("pagerduty: %w", err)
	}
	return nil
}
