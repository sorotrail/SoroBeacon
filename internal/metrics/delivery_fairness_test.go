package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliveryFairnessMetrics(t *testing.T) {
	var nilMetrics *Metrics
	nilMetrics.SetNotifyWorkspaceBacklog("acme", "first_attempt", 3, 5)
	nilMetrics.SetNotifyChannelBacklog(42, 1, 4)
	nilMetrics.RecordNotifyFairnessOverflow("acme", "workspace_queue_full")

	m := New().WithNetwork("testnet")
	m.SetNotifyWorkspaceBacklog("acme", "first_attempt", 3, 5)
	m.SetNotifyWorkspaceBacklog("acme", "retry", 1, 2)
	m.SetNotifyChannelBacklog(42, 1, 4)
	m.RecordNotifyFairnessOverflow("acme", "workspace_queue_full")
	m.RecordNotifyFairnessOverflow("acme", "retry_queue_full")
	m.RecordNotifyFairnessOverflow("acme", "channel_queue_full")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	hashedAcme := HashWorkspaceLabel("acme")
	assert.Contains(t, body, `sorobeacon_notify_workspace_in_flight{lane="first_attempt",network="testnet",workspace="`+hashedAcme+`"} 3`)
	assert.Contains(t, body, `sorobeacon_notify_workspace_queue_depth{lane="first_attempt",network="testnet",workspace="`+hashedAcme+`"} 5`)
	assert.Contains(t, body, `sorobeacon_notify_workspace_in_flight{lane="retry",network="testnet",workspace="`+hashedAcme+`"} 1`)
	assert.Contains(t, body, `sorobeacon_notify_workspace_queue_depth{lane="retry",network="testnet",workspace="`+hashedAcme+`"} 2`)
	assert.Contains(t, body, `sorobeacon_notify_channel_in_flight{channel_id="42",network="testnet"} 1`)
	assert.Contains(t, body, `sorobeacon_notify_channel_queue_depth{channel_id="42",network="testnet"} 4`)
	assert.Contains(t, body, `sorobeacon_notify_fairness_overflow_total{network="testnet",reason="workspace_queue_full",workspace="`+hashedAcme+`"} 1`)
	assert.Contains(t, body, `sorobeacon_notify_fairness_overflow_total{network="testnet",reason="retry_queue_full",workspace="`+hashedAcme+`"} 1`)
	assert.Contains(t, body, `sorobeacon_notify_fairness_overflow_total{network="testnet",reason="channel_queue_full",workspace="`+hashedAcme+`"} 1`)
	assert.NotContains(t, body, `workspace="acme"`)
}
