package alerts

import (
	"fmt"
	"time"
)

// GroupKey uniquely identifies a group of alerts. It is composed of
// (monitor_id, rule_id, contract_id) so that each distinct rule
// monitoring a specific contract gets its own grouping window.
type GroupKey struct {
	MonitorID  int64
	RuleID     int64
	ContractID string
}

// String returns the GroupKey as a colon-separated string suitable
// for use as a database key.
func (k GroupKey) String() string {
	return fmt.Sprintf("%d:%d:%s", k.MonitorID, k.RuleID, k.ContractID)
}

// MakeGroupKey constructs a GroupKey from its components.
func MakeGroupKey(monitorID int64, ruleID int64, contractID string) GroupKey {
	return GroupKey{MonitorID: monitorID, RuleID: ruleID, ContractID: contractID}
}

// WindowInfo describes the bounds and current count of an alert group.
type WindowInfo struct {
	Count      int64     `json:"count"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
}

// ShouldDeliver returns true when count is 1: the first alert in the
// window should be delivered immediately. Subsequent alerts in the
// same window are suppressed until the summary fires.
func ShouldDeliver(count int64) bool {
	return count == 1
}

// WindowBounds returns the start and end of a fixed tumbling window
// anchored at windowStart with the given duration. A zero duration
// means no window (grouping is disabled).
func WindowBounds(windowStart time.Time, windowDuration time.Duration) (time.Time, time.Time) {
	if windowDuration <= 0 {
		return windowStart, time.Time{}
	}
	return windowStart, windowStart.Add(windowDuration)
}

// GroupState holds the persisted state of an alert group.
type GroupState struct {
	GroupKey     string    `json:"group_key"`
	WindowStart  time.Time `json:"window_start"`
	Count        int64     `json:"count"`
	FirstAlertID int64     `json:"first_alert_id"`
}
