package metrics

import (
	"strings"
	"time"
)

// SetAlertPartitionRunway sets the gauge reporting how many consecutive future
// monthly alert partitions currently exist beyond the current UTC month.
func (m *Metrics) SetAlertPartitionRunway(months int) {
	if m == nil || m.alertPartitionRunwayMonths == nil {
		return
	}
	m.alertPartitionRunwayMonths.Set(float64(months))
}

// RecordAlertPartitionError increments the error counter for a partition
// lifecycle operation ("ensure", "drop", or "inspect").
func (m *Metrics) RecordAlertPartitionError(operation string) {
	if m == nil || m.alertPartitionErrorsTotal == nil {
		return
	}
	if operation == "" {
		operation = "unknown"
	}
	m.alertPartitionErrorsTotal.WithLabelValues(operation).Inc()
}

// RecordAlertPartitionsCreated increments the total count of created alert
// partitions.
func (m *Metrics) RecordAlertPartitionsCreated(n int) {
	if m == nil || m.alertPartitionsCreatedTotal == nil || n <= 0 {
		return
	}
	m.alertPartitionsCreatedTotal.Add(float64(n))
}

// RecordAlertPartitionsDropped increments the total count of dropped expired
// alert partitions.
func (m *Metrics) RecordAlertPartitionsDropped(n int) {
	if m == nil || m.alertPartitionsDroppedTotal == nil || n <= 0 {
		return
	}
	m.alertPartitionsDroppedTotal.Add(float64(n))
}

// RecordWorkspaceAlertsPruned records expired alerts deleted by the retention
// pruner for a workspace, labeled with the policy source ("workspace" or "instance").
func (m *Metrics) RecordWorkspaceAlertsPruned(workspaceID, policySource string, n int64) {
	if m == nil || m.alertPrunerDeletedTotal == nil || n < 0 {
		return
	}
	ws := strings.TrimSpace(workspaceID)
	if ws == "" {
		ws = "default"
	}
	src := strings.TrimSpace(policySource)
	if src == "" {
		src = "instance"
	}
	m.alertPrunerDeletedTotal.WithLabelValues(ws, src).Add(float64(n))
}

// SetWorkspaceRetentionSeconds sets the effective alert retention duration in
// seconds for a workspace and policy source ("workspace", "instance", or "none").
func (m *Metrics) SetWorkspaceRetentionSeconds(workspaceID, policySource string, retention time.Duration) {
	if m == nil || m.workspaceRetentionSeconds == nil {
		return
	}
	ws := strings.TrimSpace(workspaceID)
	if ws == "" {
		ws = "default"
	}
	src := strings.TrimSpace(policySource)
	if src == "" {
		src = "none"
	}
	m.workspaceRetentionSeconds.WithLabelValues(ws, src).Set(retention.Seconds())
}
