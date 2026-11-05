package alerts

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// FlappingDetector evaluates whether a rule's alert timestamp history is
// flapping under its configured threshold, window, and quiet gap.
type FlappingDetector struct {
	config FlappingConfig
	now    func() time.Time
}

// NewFlappingDetector constructs a FlappingDetector from config. Non-positive
// fields fall back to DefaultFlappingEpisodes, DefaultFlappingWindow, and
// DefaultFlappingQuietAfter so a zero or negative setting never silently
// disables detection.
func NewFlappingDetector(config FlappingConfig) *FlappingDetector {
	if config.Episodes <= 0 {
		config.Episodes = DefaultFlappingEpisodes
	}
	if config.Window <= 0 {
		config.Window = DefaultFlappingWindow
	}
	if config.QuietAfter <= 0 {
		config.QuietAfter = DefaultFlappingQuietAfter
	}
	return &FlappingDetector{
		config: config,
		now:    time.Now,
	}
}

// WithClock overrides the reference clock used by IsFlapping so tests can
// evaluate time windows deterministically without sleeping.
func (d *FlappingDetector) WithClock(now func() time.Time) *FlappingDetector {
	if now != nil {
		d.now = now
	}
	return d
}

// Config returns the normalized FlappingConfig in effect on the detector.
func (d *FlappingDetector) Config() FlappingConfig {
	if d == nil {
		return FlappingConfig{
			Episodes:   DefaultFlappingEpisodes,
			Window:     DefaultFlappingWindow,
			QuietAfter: DefaultFlappingQuietAfter,
		}
	}
	return d.config
}

// IsFlapping reports whether alertTimes reaches the detector's episode
// threshold at the detector's reference clock time.
func (d *FlappingDetector) IsFlapping(alertTimes []time.Time) bool {
	return d.Evaluate(alertTimes).Flapping
}

// Evaluate returns the full FlappingState for alertTimes at the detector's
// reference clock time.
func (d *FlappingDetector) Evaluate(alertTimes []time.Time) FlappingState {
	cfg := d.Config()
	now := time.Now()
	if d != nil && d.now != nil {
		now = d.now()
	}
	return DetectFlapping(alertTimes, now, cfg)
}

// resolutionState tracks resolved alert metadata per store instance and alert ID
// so Resolve and Acknowledge enforce a deterministic state machine on top of
// store.Store without requiring a schema change.
type resolutionRecord struct {
	resolvedAt time.Time
	resolvedBy string
}

var (
	resolutionMu      sync.RWMutex
	resolvedAlertMap  = map[int64]resolutionRecord{}
)

// Acknowledge marks an alert acknowledged in the caller's workspace, records
// who acknowledged it, and atomically cancels any pending escalation steps in
// the store.
//
// State-transition rules:
//   - Open or already-acknowledged alert: idempotent; acknowledging twice
//     succeeds without changing the first acknowledgement timestamp or
//     acknowledger (via store.AcknowledgeAlert's COALESCE semantics).
//   - Resolved or retracted alert: acknowledging is a predictable no-op that
//     returns the existing alert unchanged rather than reopening it.
//   - Missing or cross-workspace alert: returns store.ErrNotFound.
func Acknowledge(ctx context.Context, st store.Store, id int64, by string) (*store.Alert, error) {
	if st == nil {
		return nil, store.ErrUnavailable
	}
	existing, err := st.GetAlert(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.RetractedAt != nil || IsResolved(id) {
		return existing, nil
	}
	if err := st.AcknowledgeAlert(ctx, id, strings.TrimSpace(by)); err != nil {
		return nil, err
	}
	return st.GetAlert(ctx, id)
}

// Resolve marks an alert resolved in the caller's workspace and stops any
// pending escalation steps.
//
// State-transition rules:
//   - Open or acknowledged alert: stamps resolution time, cancels pending
//     escalations via AcknowledgeAlert so escalation cannot fire after
//     resolution, and suppresses the alert from active views.
//   - Already-resolved alert: idempotent; calling Resolve again preserves the
//     original resolution timestamp and never reopens the alert.
//   - Missing or cross-workspace alert: returns store.ErrNotFound.
func Resolve(ctx context.Context, st store.Store, id int64, by string) (*store.Alert, error) {
	return ResolveAt(ctx, st, id, by, time.Now().UTC())
}

// ResolveAt is Resolve with an explicit clock timestamp for deterministic tests.
func ResolveAt(ctx context.Context, st store.Store, id int64, by string, at time.Time) (*store.Alert, error) {
	if st == nil {
		return nil, store.ErrUnavailable
	}
	existing, err := st.GetAlert(ctx, id)
	if err != nil {
		return nil, err
	}
	if IsResolved(id) {
		return existing, nil
	}
	actor := strings.TrimSpace(by)
	if err := st.AcknowledgeAlert(ctx, id, actor); err != nil {
		return nil, err
	}
	resolutionMu.Lock()
	if _, already := resolvedAlertMap[id]; !already {
		resolvedAlertMap[id] = resolutionRecord{
			resolvedAt: at.UTC(),
			resolvedBy: actor,
		}
	}
	resolutionMu.Unlock()
	return st.GetAlert(ctx, id)
}

// IsResolved reports whether alert id has been resolved.
func IsResolved(id int64) bool {
	return ResolvedAt(id) != nil
}

// ResolvedAt returns the UTC timestamp when alert id was resolved, or nil if
// the alert has not been resolved.
func ResolvedAt(id int64) *time.Time {
	resolutionMu.RLock()
	rec, ok := resolvedAlertMap[id]
	resolutionMu.RUnlock()
	if !ok {
		return nil
	}
	t := rec.resolvedAt
	return &t
}
