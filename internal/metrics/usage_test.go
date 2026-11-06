package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageMetrics_OpaqueHashAndCardinalityBound(t *testing.T) {
	m := New()
	m.SetMaxTrackedWorkspaces(3)

	rawNames := []string{"tenant-alpha-secret", "tenant-bravo-secret", "tenant-charlie-secret", "tenant-delta-overflow", "tenant-echo-overflow"}
	for _, ws := range rawNames {
		m.RecordWorkspaceAlertRaised(ws, 1)
		m.RecordWorkspaceRuleEvaluations(ws, 2)
		m.RecordWorkspaceNotification(ws, "sms", "ok", 1)
		m.RecordWorkspaceNotification(ws, "voice", "error", 1)
		m.RecordWorkspaceAPIRequest(ws, 5)
		m.SetWorkspaceStoredRows(ws, "monitors", 3)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// Raw tenant names must never appear in Prometheus output.
	for _, raw := range rawNames {
		assert.NotContains(t, body, raw, "raw workspace name %q must never be exposed in /metrics", raw)
	}

	// First 3 workspaces get deterministic opaque ws_<hex> labels.
	for i := 0; i < 3; i++ {
		lbl := HashWorkspaceLabel(rawNames[i])
		assert.True(t, strings.HasPrefix(lbl, "ws_"))
		assert.Contains(t, body, fmt.Sprintf(`sorobeacon_workspace_alerts_raised_total{workspace=%q} 1`, lbl))
		assert.Contains(t, body, fmt.Sprintf(`sorobeacon_workspace_notifications_total{channel_type="sms",outcome="ok",workspace=%q} 1`, lbl))
	}

	// 4th and 5th workspaces are routed into the "overflow" bucket (1 + 1 = 2 alerts raised).
	assert.Contains(t, body, `sorobeacon_workspace_alerts_raised_total{workspace="overflow"} 2`)
	assert.Contains(t, body, `sorobeacon_workspace_rule_evaluations_total{workspace="overflow"} 4`)
	assert.Contains(t, body, `sorobeacon_workspace_notifications_total{channel_type="sms",outcome="ok",workspace="overflow"} 2`)
	assert.Contains(t, body, `sorobeacon_workspace_notifications_total{channel_type="voice",outcome="error",workspace="overflow"} 2`)
	assert.Contains(t, body, `sorobeacon_workspace_api_requests_total{workspace="overflow"} 10`)
}
