package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuleIsolationMetrics(t *testing.T) {
	var nilMetrics *Metrics
	nilMetrics.RecordRuleIsolatedEval(1, "event_emitted", 5*time.Millisecond)
	nilMetrics.RecordRuleEvalFailure(1, "event_emitted", "timeout")
	nilMetrics.RecordRuleQuarantined(1, "event_emitted", "timeout", true, 1)
	nilMetrics.RecordWorkspaceRuleBudgetExhausted("acme")

	m := New().WithNetwork("testnet")
	m.RecordRuleIsolatedEval(42, "topic_regex", 15*time.Millisecond)
	m.RecordRuleEvalFailure(42, "topic_regex", "timeout")
	m.RecordRuleEvalFailure(99, "value_threshold", "panic")
	m.RecordRuleQuarantined(42, "topic_regex", "timeout", true, 1)
	m.RecordWorkspaceRuleBudgetExhausted("acme")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, `sorobeacon_rule_evaluation_duration_seconds_count{network="testnet",rule_id="42",rule_type="topic_regex"} 1`)
	assert.Contains(t, body, `sorobeacon_rule_evaluation_failures_total{network="testnet",reason="timeout",rule_id="42",rule_type="topic_regex"} 1`)
	assert.Contains(t, body, `sorobeacon_rule_evaluation_failures_total{network="testnet",reason="panic",rule_id="99",rule_type="value_threshold"} 1`)
	assert.Contains(t, body, `sorobeacon_rule_evaluation_panics_total{network="testnet",rule_id="99",rule_type="value_threshold"} 1`)
	assert.Contains(t, body, `sorobeacon_poll_panics_total{network="testnet"} 1`)
	assert.Contains(t, body, `sorobeacon_rules_quarantined{network="testnet"} 1`)
	assert.Contains(t, body, `sorobeacon_rule_quarantined{network="testnet",rule_id="42",rule_type="topic_regex"} 1`)
	assert.Contains(t, body, `sorobeacon_rule_quarantine_transitions_total{network="testnet",reason="timeout",rule_type="topic_regex"} 1`)

	hashedAcme := HashWorkspaceLabel("acme")
	assert.Contains(t, body, `sorobeacon_workspace_rule_budget_exhausted_total{network="testnet",workspace="`+hashedAcme+`"} 1`)
	assert.NotContains(t, body, `workspace="acme"`)
}
