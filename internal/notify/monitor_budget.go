package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// DefaultMonitorCapMaxAlerts is the default number of notifications allowed per monitor per window.
const DefaultMonitorCapMaxAlerts = 50

// DefaultMonitorCapWindow is the rolling window duration for monitor notification caps.
const DefaultMonitorCapWindow = 10 * time.Minute

// DefaultMonitorCapHysteresisPercent is the volume reduction percentage required to recover.
// A value of 50 means volume must fall to 50% of the cap before unthrottling.
const DefaultMonitorCapHysteresisPercent = 50

// DefaultMonitorCapCriticalAllowance is the separate cap for critical severity alerts.
const DefaultMonitorCapCriticalAllowance = 200

// CapOutcome defines what the dispatcher should do with an alert subject to a monitor volume cap.
type CapOutcome int

const (
	// CapOutcomeAllow permits the alert to be delivered normally.
	CapOutcomeAllow CapOutcome = iota
	// CapOutcomeSummary marks the alert as the singular summary notification produced when
	// reaching the cap. The alert carries a digest explaining the flood and subsequent throttling.
	CapOutcomeSummary
	// CapOutcomeSuppressed indicates the alert exceeds the volume cap and must be suppressed.
	CapOutcomeSuppressed
)

// CapAction is the decision returned by MonitorBudgetTracker.
type CapAction struct {
	Outcome CapOutcome
	Count   int
	Window  time.Duration
}

// MonitorCapConfig defines the limits and hysteresis for monitor notification volume capping.
type MonitorCapConfig struct {
	// MaxAlerts is the maximum number of notifications per monitor inside Window.
	MaxAlerts int
	// Window is the rolling window duration.
	Window time.Duration
	// HysteresisPercent is the percentage of MaxAlerts volume must drop to before unthrottling.
	HysteresisPercent int
	// CriticalAllowance is the dedicated volume cap for critical-severity alerts.
	CriticalAllowance int
}

// Normalized ensures valid defaults for all configuration fields.
func (c MonitorCapConfig) Normalized() MonitorCapConfig {
	if c.MaxAlerts <= 0 {
		c.MaxAlerts = DefaultMonitorCapMaxAlerts
	}
	if c.Window <= 0 {
		c.Window = DefaultMonitorCapWindow
	}
	if c.HysteresisPercent <= 0 || c.HysteresisPercent >= 100 {
		c.HysteresisPercent = DefaultMonitorCapHysteresisPercent
	}
	if c.CriticalAllowance <= 0 {
		c.CriticalAllowance = DefaultMonitorCapCriticalAllowance
	}
	return c
}

type monitorBucket struct {
	timestamps         []time.Time
	criticalTimestamps []time.Time
	throttled          bool
}

// MonitorBudgetTracker tracks notification volume per monitor across sliding windows,
// enforcing caps with hysteresis recovery and separate critical allowances.
// It isolates capacity per workspace to preserve multi-tenant fairness.
type MonitorBudgetTracker struct {
	mu        sync.Mutex
	defaultCfg MonitorCapConfig
	overrides map[string]MonitorCapConfig
	buckets   map[string]*monitorBucket
}

// NewMonitorBudgetTracker creates a tracker with default configuration.
func NewMonitorBudgetTracker(cfg MonitorCapConfig) *MonitorBudgetTracker {
	return &MonitorBudgetTracker{
		defaultCfg: cfg.Normalized(),
		overrides:  make(map[string]MonitorCapConfig),
		buckets:    make(map[string]*monitorBucket),
	}
}

// SetOverride configures a custom volume cap for a specific monitor within a workspace.
func (t *MonitorBudgetTracker) SetOverride(ws string, monitorID int64, cfg MonitorCapConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := budgetKey(ws, monitorID)
	t.overrides[key] = cfg.Normalized()
}

// RecordAlert checks whether an alert should be allowed, summarized, or suppressed.
// It prunes expired events from the sliding window and enforces hysteresis on recovery.
func (t *MonitorBudgetTracker) RecordAlert(ctx context.Context, a Alert, now time.Time) CapAction {
	if a.MonitorID <= 0 {
		return CapAction{Outcome: CapOutcomeAllow}
	}

	wsID := workspace.Default.String()
	if ws, ok := workspace.From(ctx); ok && ws != "" {
		wsID = ws.String()
	}

	key := budgetKey(wsID, a.MonitorID)

	t.mu.Lock()
	defer t.mu.Unlock()

	cfg := t.defaultCfg
	if override, ok := t.overrides[key]; ok {
		cfg = override
	}

	b, exists := t.buckets[key]
	if !exists {
		b = &monitorBucket{}
		t.buckets[key] = b
	}

	cutoff := now.Add(-cfg.Window)

	// Prune regular timestamps
	valid := b.timestamps[:0]
	for _, ts := range b.timestamps {
		if !ts.Before(cutoff) {
			valid = append(valid, ts)
		}
	}
	b.timestamps = valid

	// Prune critical timestamps
	validCrit := b.criticalTimestamps[:0]
	for _, ts := range b.criticalTimestamps {
		if !ts.Before(cutoff) {
			validCrit = append(validCrit, ts)
		}
	}
	b.criticalTimestamps = validCrit

	// Check if this is a critical alert.
	// Critical alerts respect a separate critical allowance so life-safety/treasury
	// alerts are not suppressed alongside routine telemetry storms.
	if strings.EqualFold(a.Severity, "critical") {
		if len(b.criticalTimestamps) >= cfg.CriticalAllowance {
			return CapAction{
				Outcome: CapOutcomeSuppressed,
				Count:   len(b.criticalTimestamps),
				Window:  cfg.Window,
			}
		}
		b.criticalTimestamps = append(b.criticalTimestamps, now)
		return CapAction{
			Outcome: CapOutcomeAllow,
			Count:   len(b.criticalTimestamps),
			Window:  cfg.Window,
		}
	}

	// Normal (info / warning) alerts
	currentCount := len(b.timestamps)

	// Check for recovery with hysteresis
	if b.throttled {
		recoveryThreshold := (cfg.MaxAlerts * cfg.HysteresisPercent) / 100
		if recoveryThreshold < 1 {
			recoveryThreshold = 1
		}
		if currentCount <= recoveryThreshold {
			b.throttled = false
		} else {
			return CapAction{
				Outcome: CapOutcomeSuppressed,
				Count:   currentCount,
				Window:  cfg.Window,
			}
		}
	}

	// Not currently throttled
	if currentCount+1 == cfg.MaxAlerts {
		// Reached the cap! Produce the single summary alert.
		b.throttled = true
		b.timestamps = append(b.timestamps, now)
		return CapAction{
			Outcome: CapOutcomeSummary,
			Count:   currentCount + 1,
			Window:  cfg.Window,
		}
	}

	if currentCount+1 > cfg.MaxAlerts {
		b.throttled = true
		return CapAction{
			Outcome: CapOutcomeSuppressed,
			Count:   currentCount,
			Window:  cfg.Window,
		}
	}

	// Within allowance
	b.timestamps = append(b.timestamps, now)
	return CapAction{
		Outcome: CapOutcomeAllow,
		Count:   currentCount + 1,
		Window:  cfg.Window,
	}
}

// IsThrottled reports whether a monitor is currently in a throttled state.
func (t *MonitorBudgetTracker) IsThrottled(ws string, monitorID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.buckets[budgetKey(ws, monitorID)]
	if !ok {
		return false
	}
	return b.throttled
}

func budgetKey(ws string, monitorID int64) string {
	if ws == "" {
		ws = workspace.Default.String()
	}
	return fmt.Sprintf("%s:%d", ws, monitorID)
}
