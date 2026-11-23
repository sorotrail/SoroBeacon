package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// opsgenieConfig holds the configuration for the Opsgenie notification channel.
type opsgenieConfig struct {
	APIKey     string   `json:"api_key"`
	Region     string   `json:"region,omitempty"`
	Priority   string   `json:"priority,omitempty"`
	Responders []string `json:"responders,omitempty"`
	APIBase    string   `json:"api_base,omitempty"`
	baseURL    string
}

// opsgeniePriorities are the valid priority values.
var opsgeniePriorities = map[string]bool{
	"P1": true,
	"P2": true,
	"P3": true,
	"P4": true,
	"P5": true,
}

// opsgenieRegions are the valid region values.
var opsgenieRegions = map[string]string{
	"us": "https://api.opsgenie.com",
	"eu": "https://api.eu.opsgenie.com",
}

const defaultOpsgenieRegion = "us"
const defaultOpsgeniePriority = "P3"

// Opsgenie creates alerts through the Opsgenie Alerts API using GenieKey authorization.
type Opsgenie struct {
	cfg opsgenieConfig
}

// NewOpsgenie builds an Opsgenie notifier from channel config.
func NewOpsgenie(config json.RawMessage) (Notifier, error) {
	var cfg opsgenieConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("opsgenie: invalid config: %w", err)
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("opsgenie: api_key is required")
	}
	if cfg.Region == "" {
		cfg.Region = defaultOpsgenieRegion
	}
	baseURL, ok := opsgenieRegions[cfg.Region]
	if !ok {
		return nil, fmt.Errorf("opsgenie: invalid region %q (want us or eu)", cfg.Region)
	}
	if cfg.APIBase != "" {
		baseURL = strings.TrimRight(cfg.APIBase, "/")
	}
	cfg.baseURL = baseURL
	if cfg.Priority == "" {
		cfg.Priority = defaultOpsgeniePriority
	}
	if !opsgeniePriorities[cfg.Priority] {
		return nil, fmt.Errorf("opsgenie: invalid priority %q (want P1..P5)", cfg.Priority)
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	return &Opsgenie{cfg: cfg}, nil
}

// alias returns a deterministic alias for a given (rule_id, event_id) pair.
// This allows Opsgenie to deduplicate repeated alerts for the same condition.
//
// A federated alert has no local rule, so the alias carries the instance it
// came from as well: without it, two peers forwarding the same event id
// would share one alert and the second would update the first.
func (o *Opsgenie) alias(a Alert) string {
	if a.FederationOrigin != "" {
		return fmt.Sprintf("sorobeacon-%s-%d-%s", a.FederationOrigin, a.RuleID, a.EventID)
	}
	return fmt.Sprintf("sorobeacon-%d-%s", a.RuleID, a.EventID)
}

func (o *Opsgenie) Send(ctx context.Context, a Alert) error {
	details := map[string]string{
		"monitor_id":   fmt.Sprintf("%d", a.MonitorID),
		"monitor_name": a.MonitorName,
		"rule_id":      fmt.Sprintf("%d", a.RuleID),
		"rule_type":    a.RuleType,
		"event_id":     a.EventID,
		"contract_id":  a.ContractID,
		"event_name":   a.EventName,
		"ledger":       fmt.Sprintf("%d", a.Ledger),
		"tx_hash":      a.TxHash,
	}
	if a.RunbookURL != "" {
		details["runbook_url"] = a.RunbookURL
	}
	if a.ResponseGuidance != "" {
		details["response_guidance"] = a.ResponseGuidance
	}

	// Build the alert payload
	payload := map[string]any{
		"message":     fmt.Sprintf("SoroBeacon alert: %s", a.MonitorName),
		"alias":       o.alias(a),
		"description": buildDescription(a),
		"priority":    o.cfg.Priority,
		"details":     details,
	}

	if len(o.cfg.Responders) > 0 {
		payload["responders"] = o.cfg.Responders
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	headers := map[string]string{
		"Authorization": "GenieKey " + o.cfg.APIKey,
	}

	url := o.cfg.baseURL + "/v2/alerts"
	if err := postJSON(ctx, url, body, headers); err != nil {
		return fmt.Errorf("opsgenie: %w", err)
	}
	return nil
}

func buildDescription(a Alert) string {
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
	if a.RunbookURL != "" {
		fmt.Fprintf(&b, "Runbook: %s\n", a.RunbookURL)
	}
	if a.ResponseGuidance != "" {
		fmt.Fprintf(&b, "Guidance: %s\n", a.ResponseGuidance)
	}
	return b.String()
}
