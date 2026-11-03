package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

const (
	// MaxTrackedWorkspaces caps the number of distinct workspace series exported
	// to Prometheus. Any additional workspaces beyond this cap are aggregated
	// under OverflowWorkspaceLabel ("overflow") so metric cardinality stays
	// strictly bounded regardless of how many workspaces exist in the store.
	MaxTrackedWorkspaces = 256

	// OverflowWorkspaceLabel is the Prometheus workspace label used once
	// MaxTrackedWorkspaces distinct workspaces have already been registered.
	OverflowWorkspaceLabel = "overflow"
)

// HashWorkspaceLabel returns a deterministic, opaque Prometheus label for
// workspaceID ("ws_" followed by 12 hex characters of SHA-256). Raw workspace
// names are never exposed in Prometheus labels.
func HashWorkspaceLabel(workspaceID string) string {
	ws := strings.TrimSpace(workspaceID)
	if ws == "" {
		ws = "default"
	}
	sum := sha256.Sum256([]byte(ws))
	return "ws_" + hex.EncodeToString(sum[:6])
}

type workspaceLabelTracker struct {
	mu      sync.RWMutex
	max     int
	tracked map[string]string
}

func newWorkspaceLabelTracker(max int) *workspaceLabelTracker {
	if max <= 0 {
		max = MaxTrackedWorkspaces
	}
	return &workspaceLabelTracker{
		max:     max,
		tracked: make(map[string]string),
	}
}

func (t *workspaceLabelTracker) resolve(workspaceID string) string {
	ws := strings.TrimSpace(workspaceID)
	if ws == "" {
		ws = "default"
	}
	if t == nil {
		return HashWorkspaceLabel(ws)
	}

	t.mu.RLock()
	if lbl, ok := t.tracked[ws]; ok {
		t.mu.RUnlock()
		return lbl
	}
	t.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	if lbl, ok := t.tracked[ws]; ok {
		return lbl
	}
	if len(t.tracked) >= t.max {
		return OverflowWorkspaceLabel
	}
	lbl := HashWorkspaceLabel(ws)
	t.tracked[ws] = lbl
	return lbl
}

// SetMaxTrackedWorkspaces overrides the workspace cardinality cap for testing.
func (m *Metrics) SetMaxTrackedWorkspaces(max int) {
	if m == nil || m.workspaceTracker == nil {
		return
	}
	m.workspaceTracker.mu.Lock()
	defer m.workspaceTracker.mu.Unlock()
	if max <= 0 {
		max = MaxTrackedWorkspaces
	}
	m.workspaceTracker.max = max
	m.workspaceTracker.tracked = make(map[string]string)
}

// WorkspaceMetricLabel returns the bounded, opaque Prometheus label for workspaceID.
func (m *Metrics) WorkspaceMetricLabel(workspaceID string) string {
	if m == nil || m.workspaceTracker == nil {
		return HashWorkspaceLabel(workspaceID)
	}
	return m.workspaceTracker.resolve(workspaceID)
}

// RecordWorkspaceAlertRaised increments the per-workspace alerts raised counter.
func (m *Metrics) RecordWorkspaceAlertRaised(workspaceID string, n int64) {
	if m == nil || m.workspaceAlertsRaised == nil || n <= 0 {
		return
	}
	lbl := m.WorkspaceMetricLabel(workspaceID)
	m.workspaceAlertsRaised.WithLabelValues(lbl).Add(float64(n))
}

// RecordWorkspaceRuleEvaluations increments the per-workspace rule evaluations counter.
func (m *Metrics) RecordWorkspaceRuleEvaluations(workspaceID string, n int64) {
	if m == nil || m.workspaceRuleEvaluations == nil || n <= 0 {
		return
	}
	lbl := m.WorkspaceMetricLabel(workspaceID)
	m.workspaceRuleEvaluations.WithLabelValues(lbl).Add(float64(n))
}

// RecordWorkspaceNotification increments the per-workspace notification counter
// partitioned by channel_type and outcome (e.g. ok, error, throttled).
func (m *Metrics) RecordWorkspaceNotification(workspaceID, channelType, outcome string, n int64) {
	if m == nil || m.workspaceNotifications == nil || n <= 0 {
		return
	}
	chType := strings.TrimSpace(channelType)
	if chType == "" {
		chType = "unknown"
	}
	out := strings.TrimSpace(outcome)
	if out == "" {
		out = "unknown"
	}
	lbl := m.WorkspaceMetricLabel(workspaceID)
	m.workspaceNotifications.WithLabelValues(lbl, chType, out).Add(float64(n))
}

// RecordWorkspaceAPIRequest increments the per-workspace API requests counter.
func (m *Metrics) RecordWorkspaceAPIRequest(workspaceID string, n int64) {
	if m == nil || m.workspaceAPIRequests == nil || n <= 0 {
		return
	}
	lbl := m.WorkspaceMetricLabel(workspaceID)
	m.workspaceAPIRequests.WithLabelValues(lbl).Add(float64(n))
}

// SetWorkspaceStoredRows sets the exact stored row count gauge for a workspace
// and resource kind (monitors, rules, channels, alerts, dead_letters).
func (m *Metrics) SetWorkspaceStoredRows(workspaceID, resource string, count int64) {
	if m == nil || m.workspaceStoredRows == nil {
		return
	}
	res := strings.TrimSpace(resource)
	if res == "" {
		res = "unknown"
	}
	lbl := m.WorkspaceMetricLabel(workspaceID)
	m.workspaceStoredRows.WithLabelValues(lbl, res).Set(float64(count))
}
