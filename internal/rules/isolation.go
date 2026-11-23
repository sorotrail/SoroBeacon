package rules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultRuleEvalTimeout bounds the wall-clock evaluation budget of a
	// single rule against a single event. A rule that exceeds this budget is
	// aborted for that event and recorded as a timeout strike rather than
	// stalling evaluation for every other rule in the poll cycle.
	DefaultRuleEvalTimeout = 100 * time.Millisecond

	// DefaultQuarantineThreshold is the number of consecutive rule-attributed
	// failures (panics or evaluation timeouts not caused by backing store
	// latency) after which a rule is quarantined.
	DefaultQuarantineThreshold = 3

	// DefaultMaxFrequencyMatches bounds the number of in-memory match
	// timestamps a single frequency_threshold rule tracks inside its rolling
	// window. When a wide window receives more matches than this cap, the
	// oldest timestamps are evicted so one noisy rule cannot grow memory
	// without bound.
	DefaultMaxFrequencyMatches = 10_000
)

// Quarantine and isolation sentinel errors.
var (
	// ErrRuleEvalTimeout indicates a rule exceeded its per-event evaluation
	// budget due to its own computation rather than backing store latency.
	ErrRuleEvalTimeout = errors.New("rule evaluation timed out")

	// ErrRuleQuarantined indicates a rule was skipped because repeated panics
	// or timeouts placed it in quarantine.
	ErrRuleQuarantined = errors.New("rule is quarantined")
)

// QuarantineReason classifies why a rule accumulated failure strikes or
// entered quarantine.
type QuarantineReason string

const (
	QuarantineReasonPanic   QuarantineReason = "panic"
	QuarantineReasonTimeout QuarantineReason = "timeout"
)

// RulePanicError wraps a recovered panic from a single rule evaluation so the
// poller and metrics layer can attribute the crash to the offending rule while
// keeping the ingest loop alive.
type RulePanicError struct {
	RuleID    int64
	MonitorID int64
	RuleType  string
	Value     any
	Stack     []byte
}

func (e *RulePanicError) Error() string {
	return fmt.Sprintf("rule %d (%s) panicked: %v", e.RuleID, e.RuleType, e.Value)
}

// IsRulePanic reports whether err resulted from a recovered rule panic.
func IsRulePanic(err error) bool {
	var target *RulePanicError
	return errors.As(err, &target)
}

// RuleTimeoutError reports that a rule exceeded its per-evaluation timeout
// budget. DependencyAttributed is true when backing store latency consumed the
// budget, in which case the rule is not blamed or quarantined.
type RuleTimeoutError struct {
	RuleID               int64
	MonitorID            int64
	RuleType             string
	Budget               time.Duration
	Elapsed              time.Duration
	DependencyElapsed    time.Duration
	DependencyAttributed bool
}

func (e *RuleTimeoutError) Error() string {
	if e.DependencyAttributed {
		return fmt.Sprintf("rule %d (%s) timed out after %s (dependency took %s of %s budget)",
			e.RuleID, e.RuleType, e.Elapsed, e.DependencyElapsed, e.Budget)
	}
	return fmt.Sprintf("rule %d (%s) exceeded evaluation budget of %s (took %s)",
		e.RuleID, e.RuleType, e.Budget, e.Elapsed)
}

func (e *RuleTimeoutError) Unwrap() error {
	return ErrRuleEvalTimeout
}

// QuarantinedRuleError is returned when an evaluation is skipped because the
// rule is currently quarantined.
type QuarantinedRuleError struct {
	RuleID        int64
	MonitorID     int64
	RuleType      string
	Reason        QuarantineReason
	LastError     string
	QuarantinedAt time.Time
}

func (e *QuarantinedRuleError) Error() string {
	return fmt.Sprintf("rule %d (%s) is quarantined (%s): %s",
		e.RuleID, e.RuleType, e.Reason, e.LastError)
}

func (e *QuarantinedRuleError) Unwrap() error {
	return ErrRuleQuarantined
}

// IsRuleQuarantined reports whether err indicates a quarantined rule skip.
func IsRuleQuarantined(err error) bool {
	return errors.Is(err, ErrRuleQuarantined)
}

// IsDependencyAttributed reports whether err was caused by a slow or failing
// backing store dependency rather than the rule's own logic.
func IsDependencyAttributed(err error) bool {
	var te *RuleTimeoutError
	if errors.As(err, &te) && te.DependencyAttributed {
		return true
	}
	return false
}

// RuleQuarantineState describes the current health and quarantine status of a
// single rule.
type RuleQuarantineState struct {
	RuleID              int64
	MonitorID           int64
	RuleType            string
	Quarantined         bool
	Reason              QuarantineReason
	LastError           string
	ConsecutiveFailures int
	TotalPanics         int
	TotalTimeouts       int
	QuarantinedAt       time.Time
}

// QuarantineAuditEntry is an immutable audit record emitted whenever a rule is
// quarantined or re-enabled.
type QuarantineAuditEntry struct {
	RuleID              int64
	MonitorID           int64
	RuleType            string
	Action              string
	Reason              QuarantineReason
	Detail              string
	ConsecutiveFailures int
	At                  time.Time
}

type monitorIDKey struct{}
type isolatedEvalKey struct{}
type depTrackerKey struct{}

// WithMonitorID attaches the owning monitor/workspace identifier to ctx so
// isolation logs, metrics, and quarantine records identify which monitor owns
// a pathological rule.
func WithMonitorID(ctx context.Context, monitorID int64) context.Context {
	return context.WithValue(ctx, monitorIDKey{}, monitorID)
}

// MonitorID returns the monitor id carried by ctx, or 0 when unset.
func MonitorID(ctx context.Context) int64 {
	id, _ := ctx.Value(monitorIDKey{}).(int64)
	return id
}

func inIsolatedEval(ctx context.Context) bool {
	ok, _ := ctx.Value(isolatedEvalKey{}).(bool)
	return ok
}

// dependencyTracker measures time spent waiting on external dependencies (such
// as the durable MatchLog store) during a rule evaluation so database slowdowns
// are never misattributed to the rule itself.
type dependencyTracker struct {
	mu            sync.Mutex
	accumulated   time.Duration
	inFlightStart time.Time
	depErr        error
}

func withDependencyTracker(ctx context.Context) (context.Context, *dependencyTracker) {
	t := &dependencyTracker{}
	return context.WithValue(ctx, depTrackerKey{}, t), t
}

// RecordDependencyCall wraps a backing-store operation performed inside a
// stateful rule evaluator. Time spent inside fn (including time still in flight
// if the evaluation context expires) is subtracted from the rule's compute
// budget when deciding whether a timeout should count toward quarantine.
func RecordDependencyCall(ctx context.Context, fn func(ctx context.Context) error) error {
	tracker, _ := ctx.Value(depTrackerKey{}).(*dependencyTracker)
	if tracker == nil {
		return fn(ctx)
	}
	start := time.Now()
	tracker.mu.Lock()
	tracker.inFlightStart = start
	tracker.mu.Unlock()

	err := fn(ctx)
	elapsed := time.Since(start)

	tracker.mu.Lock()
	tracker.inFlightStart = time.Time{}
	tracker.accumulated += elapsed
	if err != nil && tracker.depErr == nil {
		tracker.depErr = err
	}
	tracker.mu.Unlock()
	return err
}

func (t *dependencyTracker) snapshot(now time.Time) (time.Duration, error) {
	if t == nil {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	total := t.accumulated
	if !t.inFlightStart.IsZero() && now.After(t.inFlightStart) {
		total += now.Sub(t.inFlightStart)
	}
	return total, t.depErr
}

// ruleIsolator tracks per-rule failure strikes, quarantine states, and the
// quarantine audit trail.
type ruleIsolator struct {
	mu        sync.RWMutex
	states    map[int64]*RuleQuarantineState
	audit     []QuarantineAuditEntry
	threshold int
	now       func() time.Time
	log       *slog.Logger
}

func newRuleIsolator() *ruleIsolator {
	return &ruleIsolator{
		states:    make(map[int64]*RuleQuarantineState),
		threshold: DefaultQuarantineThreshold,
		now:       time.Now,
	}
}

func (iso *ruleIsolator) quarantineThreshold() int {
	iso.mu.RLock()
	defer iso.mu.RUnlock()
	if iso.threshold <= 0 {
		return DefaultQuarantineThreshold
	}
	return iso.threshold
}

func (iso *ruleIsolator) checkQuarantined(ruleID int64) (RuleQuarantineState, bool) {
	if ruleID == 0 {
		return RuleQuarantineState{}, false
	}
	iso.mu.RLock()
	defer iso.mu.RUnlock()
	st, ok := iso.states[ruleID]
	if !ok || !st.Quarantined {
		return RuleQuarantineState{}, false
	}
	return *st, true
}

func (iso *ruleIsolator) recordSuccess(ruleID int64) {
	if ruleID == 0 {
		return
	}
	iso.mu.Lock()
	defer iso.mu.Unlock()
	if st, ok := iso.states[ruleID]; ok && !st.Quarantined {
		st.ConsecutiveFailures = 0
	}
}

func (iso *ruleIsolator) recordStrike(ruleID, monitorID int64, ruleType string, reason QuarantineReason, detail string) RuleQuarantineState {
	iso.mu.Lock()
	defer iso.mu.Unlock()

	now := time.Now()
	if iso.now != nil {
		now = iso.now()
	}
	threshold := iso.threshold
	if threshold <= 0 {
		threshold = DefaultQuarantineThreshold
	}

	st, ok := iso.states[ruleID]
	if !ok {
		st = &RuleQuarantineState{
			RuleID:    ruleID,
			MonitorID: monitorID,
			RuleType:  ruleType,
		}
		iso.states[ruleID] = st
	}
	if monitorID != 0 {
		st.MonitorID = monitorID
	}
	if ruleType != "" {
		st.RuleType = ruleType
	}
	st.ConsecutiveFailures++
	st.Reason = reason
	st.LastError = detail
	switch reason {
	case QuarantineReasonPanic:
		st.TotalPanics++
	case QuarantineReasonTimeout:
		st.TotalTimeouts++
	}

	if !st.Quarantined && ruleID != 0 && st.ConsecutiveFailures >= threshold {
		st.Quarantined = true
		st.QuarantinedAt = now
		entry := QuarantineAuditEntry{
			RuleID:              ruleID,
			MonitorID:           st.MonitorID,
			RuleType:            st.RuleType,
			Action:              "quarantined",
			Reason:              reason,
			Detail:              detail,
			ConsecutiveFailures: st.ConsecutiveFailures,
			At:                  now,
		}
		iso.audit = append(iso.audit, entry)
		if iso.log != nil {
			iso.log.Error("rule quarantined after repeated evaluation failures",
				"rule_id", ruleID,
				"monitor_id", st.MonitorID,
				"rule_type", st.RuleType,
				"reason", string(reason),
				"consecutive_failures", st.ConsecutiveFailures,
				"last_error", detail,
			)
		}
	}
	return *st
}

func (iso *ruleIsolator) state(ruleID int64) (RuleQuarantineState, bool) {
	iso.mu.RLock()
	defer iso.mu.RUnlock()
	st, ok := iso.states[ruleID]
	if !ok {
		return RuleQuarantineState{}, false
	}
	return *st, true
}

func (iso *ruleIsolator) quarantinedList() []RuleQuarantineState {
	iso.mu.RLock()
	defer iso.mu.RUnlock()
	var out []RuleQuarantineState
	for _, st := range iso.states {
		if st.Quarantined {
			out = append(out, *st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleID < out[j].RuleID })
	return out
}

func (iso *ruleIsolator) auditLog() []QuarantineAuditEntry {
	iso.mu.RLock()
	defer iso.mu.RUnlock()
	out := make([]QuarantineAuditEntry, len(iso.audit))
	copy(out, iso.audit)
	return out
}

func (iso *ruleIsolator) reenable(ruleID int64) bool {
	iso.mu.Lock()
	defer iso.mu.Unlock()
	st, ok := iso.states[ruleID]
	if !ok || !st.Quarantined {
		return false
	}
	now := time.Now()
	if iso.now != nil {
		now = iso.now()
	}
	prevReason := st.Reason
	prevErr := st.LastError
	st.Quarantined = false
	st.ConsecutiveFailures = 0
	st.Reason = ""
	st.LastError = ""
	st.QuarantinedAt = time.Time{}

	iso.audit = append(iso.audit, QuarantineAuditEntry{
		RuleID:              ruleID,
		MonitorID:           st.MonitorID,
		RuleType:            st.RuleType,
		Action:              "reenabled",
		Reason:              prevReason,
		Detail:              prevErr,
		ConsecutiveFailures: 0,
		At:                  now,
	})
	if iso.log != nil {
		iso.log.Info("quarantined rule re-enabled",
			"rule_id", ruleID,
			"monitor_id", st.MonitorID,
			"rule_type", st.RuleType,
		)
	}
	return true
}

func recoverRulePanic(ruleID, monitorID int64, ruleType string, rec any) *RulePanicError {
	return &RulePanicError{
		RuleID:    ruleID,
		MonitorID: monitorID,
		RuleType:  ruleType,
		Value:     rec,
		Stack:     debug.Stack(),
	}
}
