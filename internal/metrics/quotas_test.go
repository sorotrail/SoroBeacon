package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceQuotaAndBudgetMetrics(t *testing.T) {
	// Nil safety
	var nilMetrics *Metrics
	nilMetrics.SetWorkspaceQuotaLimit("ws-1", "monitors", 10)
	nilMetrics.SetWorkspaceQuotaUsage("ws-1", "monitors", 5)
	nilMetrics.RecordWorkspaceQuotaRejection("ws-1", "monitors")
	nilMetrics.RecordWorkspaceQueryCostCharged("ws-1", "rest", 12)
	nilMetrics.SetWorkspaceQueryBudgetRemaining("ws-1", 88)
	nilMetrics.RecordWorkspaceQueryBudgetThrottled("ws-1", "graphql")

	m := New()
	m.SetWorkspaceQuotaLimit("ws-alpha", "monitors", 10)
	m.SetWorkspaceQuotaUsage("ws-alpha", "monitors", 10)
	m.RecordWorkspaceQuotaRejection("ws-alpha", "monitors")
	m.RecordWorkspaceQueryCostCharged("ws-alpha", "rest", 15)
	m.RecordWorkspaceQueryCostCharged("ws-alpha", "graphql", 25)
	m.SetWorkspaceQueryBudgetRemaining("ws-alpha", 60)
	m.RecordWorkspaceQueryBudgetThrottled("ws-alpha", "graphql")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	ws := HashWorkspaceLabel("ws-alpha")
	assert.Contains(t, body, `sorobeacon_workspace_quota_limit{resource="monitors",workspace="`+ws+`"} 10`)
	assert.Contains(t, body, `sorobeacon_workspace_quota_usage{resource="monitors",workspace="`+ws+`"} 10`)
	assert.Contains(t, body, `sorobeacon_workspace_quota_rejections_total{resource="monitors",workspace="`+ws+`"} 1`)
	assert.Contains(t, body, `sorobeacon_workspace_query_cost_charged_total{surface="rest",workspace="`+ws+`"} 15`)
	assert.Contains(t, body, `sorobeacon_workspace_query_cost_charged_total{surface="graphql",workspace="`+ws+`"} 25`)
	assert.Contains(t, body, `sorobeacon_workspace_query_budget_remaining{workspace="`+ws+`"} 60`)
	assert.Contains(t, body, `sorobeacon_workspace_query_budget_throttled_total{surface="graphql",workspace="`+ws+`"} 1`)
	assert.NotContains(t, body, `workspace="ws-alpha"`)
}
