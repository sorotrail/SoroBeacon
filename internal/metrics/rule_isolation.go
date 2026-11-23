package metrics

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type ruleIsolationCollectors struct {
	evalDurationByRule  *prometheus.HistogramVec
	evalFailuresByRule  *prometheus.CounterVec
	evalPanicsTotal     *prometheus.CounterVec
	quarantinedGauge    *prometheus.GaugeVec
	quarantinedByRule   *prometheus.GaugeVec
	quarantineTotal     *prometheus.CounterVec
	workspaceSkipped    *prometheus.CounterVec
}

var (
	ruleIsolationMu         sync.Mutex
	ruleIsolationByRegistry = make(map[*prometheus.Registry]*ruleIsolationCollectors)
)

func (m *Metrics) ruleIsolationSet() *ruleIsolationCollectors {
	if m == nil || m.registry == nil {
		return nil
	}
	ruleIsolationMu.Lock()
	defer ruleIsolationMu.Unlock()
	if c, ok := ruleIsolationByRegistry[m.registry]; ok {
		return c
	}
	c := &ruleIsolationCollectors{
		evalDurationByRule: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sorobeacon_rule_evaluation_duration_seconds",
			Help:    "Per-rule evaluation duration in seconds (pure rule evaluation time excluding database I/O).",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
		}, []string{"network", "rule_id", "rule_type"}),
		evalFailuresByRule: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_rule_evaluation_failures_total",
			Help: "Total rule evaluation failures attributed to a rule, labelled by rule_id, rule_type, and reason (timeout|panic|error|quarantined).",
		}, []string{"network", "rule_id", "rule_type", "reason"}),
		evalPanicsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_rule_evaluation_panics_total",
			Help: "Total panics recovered during individual rule evaluations.",
		}, []string{"network", "rule_id", "rule_type"}),
		quarantinedGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_rules_quarantined",
			Help: "Current number of rules quarantined due to repeated timeouts, panics, or evaluation errors.",
		}, []string{"network"}),
		quarantinedByRule: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_rule_quarantined",
			Help: "1 while the given rule_id is quarantined, 0 when healthy or re-enabled.",
		}, []string{"network", "rule_id", "rule_type"}),
		quarantineTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_rule_quarantine_transitions_total",
			Help: "Total transitions into quarantine per rule_type and reason.",
		}, []string{"network", "rule_type", "reason"}),
		workspaceSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_workspace_rule_budget_exhausted_total",
			Help: "Total rule evaluations deferred because a workspace exhausted its per-event evaluation time budget.",
		}, []string{"network", "workspace"}),
	}
	m.registry.MustRegister(
		c.evalDurationByRule,
		c.evalFailuresByRule,
		c.evalPanicsTotal,
		c.quarantinedGauge,
		c.quarantinedByRule,
		c.quarantineTotal,
		c.workspaceSkipped,
	)
	ruleIsolationByRegistry[m.registry] = c
	return c
}

func formatRuleIDLabel(ruleID int64) string {
	if ruleID <= 0 {
		return "0"
	}
	return strconv.FormatInt(ruleID, 10)
}

func normalizeRuleFailureReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "timeout":
		return "timeout"
	case "panic":
		return "panic"
	case "quarantined":
		return "quarantined"
	case "error":
		return "error"
	default:
		return "error"
	}
}

// RecordRuleIsolatedEval records the pure rule evaluation duration for a specific rule ID and rule type.
func (m *Metrics) RecordRuleIsolatedEval(ruleID int64, ruleType string, duration time.Duration) {
	c := m.ruleIsolationSet()
	if c == nil {
		return
	}
	if ruleType == "" {
		ruleType = "unknown"
	}
	c.evalDurationByRule.WithLabelValues(m.network, formatRuleIDLabel(ruleID), ruleType).Observe(duration.Seconds())
}

// RecordRuleEvalFailure records a per-rule failure (timeout, panic, error, or quarantined skip).
// When reason is "panic", it also increments sorobeacon_poll_panics_total via RecordPanic so
// existing panic alerts fire.
func (m *Metrics) RecordRuleEvalFailure(ruleID int64, ruleType, reason string) {
	c := m.ruleIsolationSet()
	if c == nil {
		return
	}
	if ruleType == "" {
		ruleType = "unknown"
	}
	normReason := normalizeRuleFailureReason(reason)
	idLabel := formatRuleIDLabel(ruleID)
	c.evalFailuresByRule.WithLabelValues(m.network, idLabel, ruleType, normReason).Inc()
	if normReason == "panic" {
		c.evalPanicsTotal.WithLabelValues(m.network, idLabel, ruleType).Inc()
		m.RecordPanic()
	}
}

// RecordRuleQuarantined updates the quarantine gauges and transition counter when a rule is quarantined or re-enabled.
func (m *Metrics) RecordRuleQuarantined(ruleID int64, ruleType, reason string, quarantined bool, totalQuarantined int) {
	c := m.ruleIsolationSet()
	if c == nil {
		return
	}
	if ruleType == "" {
		ruleType = "unknown"
	}
	idLabel := formatRuleIDLabel(ruleID)
	if quarantined {
		c.quarantinedByRule.WithLabelValues(m.network, idLabel, ruleType).Set(1)
		c.quarantineTotal.WithLabelValues(m.network, ruleType, normalizeRuleFailureReason(reason)).Inc()
	} else {
		c.quarantinedByRule.WithLabelValues(m.network, idLabel, ruleType).Set(0)
	}
	if totalQuarantined < 0 {
		totalQuarantined = 0
	}
	c.quarantinedGauge.WithLabelValues(m.network).Set(float64(totalQuarantined))
}

// RecordWorkspaceRuleBudgetExhausted increments the counter when a workspace's per-event evaluation budget is exceeded.
func (m *Metrics) RecordWorkspaceRuleBudgetExhausted(workspace string) {
	c := m.ruleIsolationSet()
	if c == nil {
		return
	}
	c.workspaceSkipped.WithLabelValues(m.network, HashWorkspaceLabel(workspace)).Inc()
}
