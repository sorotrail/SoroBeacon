package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

const (
	// DefaultEvalTimeout bounds how long a single rule evaluation may spend in
	// rule logic (excluding attributed database I/O) before being aborted.
	DefaultEvalTimeout = 100 * time.Millisecond

	// DefaultQuarantineThreshold is the number of consecutive rule-attributed
	// failures (timeouts, panics, or runtime evaluation errors) after which a
	// rule is quarantined so it cannot repeatedly stall or degrade the poller.
	DefaultQuarantineThreshold = 3

	// DefaultWorkspaceEvalBudget bounds how much total rule-evaluation wall
	// time a single workspace may consume per event when multiple workspaces
	// watch the same contract.
	DefaultWorkspaceEvalBudget = 500 * time.Millisecond

	// MaxFrequencyMatchesPerRule caps the number of rolling-window match
	// timestamps retained in memory for a single frequency_threshold rule.
	// When the cap is reached, the oldest timestamps are evicted so an
	// unbounded event flood or a very wide window cannot exhaust memory.
	MaxFrequencyMatchesPerRule = 10000

	// MaxTrackedStatefulRules caps the number of distinct rule IDs tracked
	// simultaneously in a stateful evaluator's in-memory map.
	MaxTrackedStatefulRules = 10000
)

var (
	// ErrRuleTimeout indicates that a rule evaluation exceeded its per-rule timeout budget.
	ErrRuleTimeout = errors.New("rule evaluation timed out")

	// ErrRulePanic indicates that a rule evaluation panicked and was recovered.
	ErrRulePanic = errors.New("rule evaluation panicked")

	// ErrRuleQuarantined indicates that a rule was skipped because it is quarantined.
	ErrRuleQuarantined = errors.New("rule is quarantined")

	// ErrWorkspaceEvalBudgetExceeded indicates that a workspace exhausted its
	// per-event evaluation budget.
	ErrWorkspaceEvalBudgetExceeded = errors.New("workspace rule evaluation budget exceeded")
)

// EvalFailureKind classifies why a rule evaluation failed.
type EvalFailureKind string

const (
	EvalFailureNone        EvalFailureKind = ""
	EvalFailureTimeout     EvalFailureKind = "timeout"
	EvalFailurePanic       EvalFailureKind = "panic"
	EvalFailureError       EvalFailureKind = "error"
	EvalFailureQuarantined EvalFailureKind = "quarantined"
	EvalFailureStorage     EvalFailureKind = "storage"
)

// RuleEvalError describes an isolated rule evaluation failure, including
// whether the failure was attributed to the rule or to underlying storage.
type RuleEvalError struct {
	RuleID      int64
	RuleType    string
	Kind        EvalFailureKind
	Duration    time.Duration
	DBDuration  time.Duration
	PanicValue  any
	Cause       error
	Quarantined bool
}

func (e *RuleEvalError) Error() string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case EvalFailurePanic:
		return fmt.Sprintf("rule %d (%s) panicked: %v", e.RuleID, e.RuleType, e.PanicValue)
	case EvalFailureTimeout:
		return fmt.Sprintf("rule %d (%s) evaluation timed out after %s (db=%s)", e.RuleID, e.RuleType, e.Duration, e.DBDuration)
	case EvalFailureQuarantined:
		return fmt.Sprintf("rule %d (%s) is quarantined: %v", e.RuleID, e.RuleType, e.Cause)
	default:
		if e.Cause != nil {
			return e.Cause.Error()
		}
		return fmt.Sprintf("rule %d (%s) evaluation failed", e.RuleID, e.RuleType)
	}
}

func (e *RuleEvalError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Kind {
	case EvalFailurePanic:
		return ErrRulePanic
	case EvalFailureTimeout:
		return ErrRuleTimeout
	case EvalFailureQuarantined:
		return ErrRuleQuarantined
	default:
		return e.Cause
	}
}

// IsRuleIsolatedFailure reports whether err is a rule-level failure (timeout,
// panic, quarantined skip, or rule logic error) rather than a transient
// database/storage failure. The poller uses this distinction so a broken rule
// is recorded and quarantined without holding the contract cursor back forever.
func IsRuleIsolatedFailure(err error) bool {
	if err == nil {
		return false
	}
	if IsStorageError(err) {
		return false
	}
	var evalErr *RuleEvalError
	if errors.As(err, &evalErr) {
		return evalErr.Kind != EvalFailureStorage
	}
	return true
}

// TransactionalRuleEvaluator is an optional extension to StatefulRuleEvaluator
// that allows the registry to snapshot a rule's in-memory state before
// evaluation and restore it atomically if the evaluation panics, times out, or
// errors midway through (including inside a composite rule).
type TransactionalRuleEvaluator interface {
	StatefulRuleEvaluator
	SnapshotRule(ruleID int64) any
	RestoreRule(ruleID int64, snapshot any)
}

type storageError struct {
	err error
}

func (s storageError) Error() string { return s.err.Error() }
func (s storageError) Unwrap() error { return s.err }

// WrapStorageError marks err as originating from database/storage I/O so that
// slow or failing database queries never count toward quarantining a rule.
func WrapStorageError(err error) error {
	if err == nil {
		return nil
	}
	return storageError{err: err}
}

// IsStorageError reports whether err was caused by database/storage I/O rather
// than by the rule's own evaluation logic.
func IsStorageError(err error) bool {
	var se storageError
	return errors.As(err, &se)
}

type dbDurationTrackerKey struct{}

type dbDurationTracker struct {
	mu         sync.Mutex
	accumNanos int64
	inFlight   int
	startedAt  time.Time
	storageErr bool
}

// WithDBTimeTracker attaches a per-evaluation database I/O timer to ctx.
func WithDBTimeTracker(ctx context.Context) context.Context {
	return context.WithValue(ctx, dbDurationTrackerKey{}, &dbDurationTracker{})
}

// EnterDB marks the start of a database/storage operation during rule
// evaluation and returns a closure that must be called when the database
// operation finishes. Time spent inside EnterDB is attributed to database I/O
// rather than to the rule's own evaluation budget.
func EnterDB(ctx context.Context) func(err error) {
	t, _ := ctx.Value(dbDurationTrackerKey{}).(*dbDurationTracker)
	if t == nil {
		return func(error) {}
	}
	now := time.Now()
	t.mu.Lock()
	if t.inFlight == 0 {
		t.startedAt = now
	}
	t.inFlight++
	t.mu.Unlock()

	var once sync.Once
	return func(err error) {
		once.Do(func() {
			end := time.Now()
			t.mu.Lock()
			if t.inFlight > 0 {
				t.inFlight--
				if t.inFlight == 0 && !t.startedAt.IsZero() {
					d := end.Sub(t.startedAt)
					if d > 0 {
						t.accumNanos += int64(d)
					}
					t.startedAt = time.Time{}
				}
			}
			if err != nil {
				t.storageErr = true
			}
			t.mu.Unlock()
		})
	}
}

// RecordDBDuration records time spent waiting on database/storage I/O during a
// rule evaluation so the isolation boundary can subtract DB latency from the
// rule's own evaluation duration.
func RecordDBDuration(ctx context.Context, d time.Duration, err error) {
	if d < 0 {
		d = 0
	}
	t, _ := ctx.Value(dbDurationTrackerKey{}).(*dbDurationTracker)
	if t == nil {
		return
	}
	t.mu.Lock()
	t.accumNanos += int64(d)
	if err != nil {
		t.storageErr = true
	}
	t.mu.Unlock()
}

// AttributedDBDuration returns the database I/O duration accumulated on ctx
// (including any currently in-flight DB call) and whether a storage error occurred.
func AttributedDBDuration(ctx context.Context) (time.Duration, bool) {
	t, _ := ctx.Value(dbDurationTrackerKey{}).(*dbDurationTracker)
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	total := time.Duration(t.accumNanos)
	if t.inFlight > 0 && !t.startedAt.IsZero() {
		if d := time.Since(t.startedAt); d > 0 {
			total += d
		}
	}
	return total, t.storageErr
}

// RuleQuarantineState holds the observable health and quarantine status of a rule.
type RuleQuarantineState struct {
	RuleID              int64
	RuleVersion         int
	RuleType            string
	ConsecutiveFailures int
	TotalFailures       int64
	TotalTimeouts       int64
	TotalPanics         int64
	Quarantined         bool
	QuarantinedAt       time.Time
	Reason              EvalFailureKind
	LastError           string
	LastDuration        time.Duration
}

// QuarantineTransitionFunc is invoked when a rule enters or leaves quarantine.
type QuarantineTransitionFunc func(ctx context.Context, state RuleQuarantineState)

// EvalObservation carries the telemetry of one completed (or skipped) rule
// evaluation for metrics and logging.
type EvalObservation struct {
	RuleID       int64
	RuleVersion  int
	RuleType     string
	Matched      bool
	RuleDuration time.Duration
	DBDuration   time.Duration
	FailureKind  EvalFailureKind
	Quarantined  bool
	Err          error
}

type inCompositeEvalKey struct{}

func withInCompositeEval(ctx context.Context) context.Context {
	return context.WithValue(ctx, inCompositeEvalKey{}, true)
}

func isInCompositeEval(ctx context.Context) bool {
	v, _ := ctx.Value(inCompositeEvalKey{}).(bool)
	return v
}

type ruleIsolationEntry struct {
	state RuleQuarantineState
}

// WithEvalTimeout configures the per-rule evaluation timeout on the registry.
// Zero disables the timeout; NewRegistry defaults to DefaultEvalTimeout.
func (r *Registry) WithEvalTimeout(d time.Duration) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evalTimeout = d
	return r
}

// EvalTimeout returns the configured per-rule evaluation timeout.
func (r *Registry) EvalTimeout() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.evalTimeout
}

// WithQuarantineThreshold sets how many consecutive rule-attributed failures
// (timeouts, panics, or runtime errors) quarantine a rule. Zero disables
// automatic quarantine; NewRegistry defaults to DefaultQuarantineThreshold.
func (r *Registry) WithQuarantineThreshold(threshold int) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quarantineThreshold = threshold
	return r
}

// QuarantineThreshold returns the configured consecutive-failure threshold.
func (r *Registry) QuarantineThreshold() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.quarantineThreshold
}

// WithQuarantineHook registers a callback invoked whenever a rule transitions
// into or out of quarantine.
func (r *Registry) WithQuarantineHook(fn QuarantineTransitionFunc) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quarantineHook = fn
	return r
}

// WithEvalObserver registers a callback invoked after every top-level rule
// evaluation with pure rule duration, DB duration, and failure classification.
func (r *Registry) WithEvalObserver(fn func(ctx context.Context, obs EvalObservation)) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evalObserver = fn
	return r
}

// QuarantineState returns the current isolation and quarantine state for ruleID.
func (r *Registry) QuarantineState(ruleID int64) (RuleQuarantineState, bool) {
	if r == nil || ruleID == 0 {
		return RuleQuarantineState{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.isolation[ruleID]
	if !ok || entry == nil {
		return RuleQuarantineState{}, false
	}
	return entry.state, true
}

// QuarantinedRules returns a snapshot of all currently quarantined rules.
func (r *Registry) QuarantinedRules() []RuleQuarantineState {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []RuleQuarantineState
	for _, entry := range r.isolation {
		if entry != nil && entry.state.Quarantined {
			out = append(out, entry.state)
		}
	}
	return out
}

// QuarantinedCount returns the number of currently quarantined rules.
func (r *Registry) QuarantinedCount() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, entry := range r.isolation {
		if entry != nil && entry.state.Quarantined {
			count++
		}
	}
	return count
}

// ReenableRule clears any quarantine and failure strikes for ruleID and resets
// its stateful evaluator state so a fixed rule can resume evaluation cleanly.
// Returns true if the rule was previously quarantined.
func (r *Registry) ReenableRule(ctx context.Context, ruleID int64) bool {
	if r == nil || ruleID == 0 {
		return false
	}
	for _, e := range r.evaluators {
		if sr, ok := e.(StatefulRuleEvaluator); ok {
			sr.ResetRule(ruleID)
		}
	}
	return r.clearIsolationState(ctx, ruleID)
}

func (r *Registry) clearIsolationState(ctx context.Context, ruleID int64) bool {
	if r == nil || ruleID == 0 {
		return false
	}
	r.mu.Lock()
	entry, ok := r.isolation[ruleID]
	if !ok || entry == nil {
		r.mu.Unlock()
		return false
	}
	wasQuarantined := entry.state.Quarantined
	clearedState := entry.state
	clearedState.Quarantined = false
	clearedState.ConsecutiveFailures = 0
	clearedState.QuarantinedAt = time.Time{}
	delete(r.isolation, ruleID)
	hook := r.quarantineHook
	r.mu.Unlock()

	if wasQuarantined && hook != nil {
		hook(ctx, clearedState)
	}
	return wasQuarantined
}

func (r *Registry) snapshotRuleStates(ruleID int64) map[string]any {
	if r == nil || ruleID == 0 {
		return nil
	}
	var snaps map[string]any
	for name, e := range r.evaluators {
		if tr, ok := e.(TransactionalRuleEvaluator); ok {
			if s := tr.SnapshotRule(ruleID); s != nil {
				if snaps == nil {
					snaps = make(map[string]any)
				}
				snaps[name] = s
			}
		}
	}
	return snaps
}

func (r *Registry) restoreRuleStates(ruleID int64, snaps map[string]any) {
	if r == nil || ruleID == 0 {
		return
	}
	for name, e := range r.evaluators {
		if tr, ok := e.(TransactionalRuleEvaluator); ok {
			tr.RestoreRule(ruleID, snaps[name])
		} else if sr, ok := e.(StatefulRuleEvaluator); ok {
			sr.ResetRule(ruleID)
		}
	}
}

func (r *Registry) checkQuarantineBeforeEval(ctx context.Context, ruleID int64, ruleVersion int, ruleType string) (*RuleEvalError, bool) {
	if ruleID == 0 {
		return nil, false
	}
	r.mu.Lock()
	entry, ok := r.isolation[ruleID]
	if !ok || entry == nil || !entry.state.Quarantined {
		r.mu.Unlock()
		return nil, false
	}
	// If the rule was updated (its version increased since quarantine),
	// automatically clear the quarantine so the operator's fix takes effect.
	if ruleVersion > 0 && entry.state.RuleVersion > 0 && ruleVersion != entry.state.RuleVersion {
		cleared := entry.state
		cleared.Quarantined = false
		cleared.ConsecutiveFailures = 0
		cleared.RuleVersion = ruleVersion
		delete(r.isolation, ruleID)
		hook := r.quarantineHook
		r.mu.Unlock()
		r.ResetRuleState(ruleID)
		if hook != nil {
			hook(ctx, cleared)
		}
		return nil, false
	}
	st := entry.state
	r.mu.Unlock()

	return &RuleEvalError{
		RuleID:      ruleID,
		RuleType:    ruleType,
		Kind:        EvalFailureQuarantined,
		Quarantined: true,
		Cause:       fmt.Errorf("%w (reason=%s, last_error=%s)", ErrRuleQuarantined, st.Reason, st.LastError),
	}, true
}

func (r *Registry) recordEvalOutcome(
	ctx context.Context,
	ruleID int64,
	ruleVersion int,
	ruleType string,
	matched bool,
	ruleDuration time.Duration,
	dbDuration time.Duration,
	failureKind EvalFailureKind,
	evalErr error,
) bool {
	var (
		transitioned bool
		qState       RuleQuarantineState
		hook         QuarantineTransitionFunc
		observer     func(context.Context, EvalObservation)
	)

	r.mu.Lock()
	observer = r.evalObserver
	threshold := r.quarantineThreshold
	hook = r.quarantineHook

	if ruleID != 0 {
		if r.isolation == nil {
			r.isolation = make(map[int64]*ruleIsolationEntry)
		}
		entry := r.isolation[ruleID]
		switch failureKind {
		case EvalFailureNone:
			// A successful evaluation resets consecutive rule strikes if the
			// rule is not quarantined.
			if entry != nil && !entry.state.Quarantined {
				entry.state.ConsecutiveFailures = 0
				entry.state.LastDuration = ruleDuration
			}
		case EvalFailureStorage:
			// Storage failures are not attributed to the rule: neither add a
			// strike nor clear existing strikes.
		case EvalFailureTimeout, EvalFailurePanic, EvalFailureError:
			if entry == nil {
				if len(r.isolation) >= MaxTrackedStatefulRules {
					for k, v := range r.isolation {
						if v == nil || !v.state.Quarantined {
							delete(r.isolation, k)
							break
						}
					}
				}
				entry = &ruleIsolationEntry{
					state: RuleQuarantineState{
						RuleID:      ruleID,
						RuleVersion: ruleVersion,
						RuleType:    ruleType,
					},
				}
				r.isolation[ruleID] = entry
			}
			entry.state.RuleType = ruleType
			if ruleVersion > 0 {
				entry.state.RuleVersion = ruleVersion
			}
			entry.state.ConsecutiveFailures++
			entry.state.TotalFailures++
			switch failureKind {
			case EvalFailureTimeout:
				entry.state.TotalTimeouts++
			case EvalFailurePanic:
				entry.state.TotalPanics++
			}
			entry.state.LastDuration = ruleDuration
			if evalErr != nil {
				entry.state.LastError = evalErr.Error()
			}
			if threshold > 0 && !entry.state.Quarantined && entry.state.ConsecutiveFailures >= threshold {
				entry.state.Quarantined = true
				entry.state.QuarantinedAt = time.Now().UTC()
				entry.state.Reason = failureKind
				transitioned = true
			}
			qState = entry.state
		case EvalFailureQuarantined:
			if entry != nil {
				qState = entry.state
			}
		}
	}
	r.mu.Unlock()

	if transitioned && hook != nil {
		hook(ctx, qState)
	}
	if observer != nil {
		observer(ctx, EvalObservation{
			RuleID:       ruleID,
			RuleVersion:  ruleVersion,
			RuleType:     ruleType,
			Matched:      matched,
			RuleDuration: ruleDuration,
			DBDuration:   dbDuration,
			FailureKind:  failureKind,
			Quarantined:  qState.Quarantined,
			Err:          evalErr,
		})
	}
	return qState.Quarantined
}

type evalCallResult struct {
	matched  bool
	err      error
	panicked bool
	panicVal any
}

func (r *Registry) evaluateIsolated(ctx context.Context, ruleType string, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	if isInCompositeEval(ctx) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return r.evaluate(ctx, ruleType, ev, params)
	}

	ruleID := RuleID(ctx)
	ruleVersion := RuleVersion(ctx)

	if qErr, quarantined := r.checkQuarantineBeforeEval(ctx, ruleID, ruleVersion, ruleType); quarantined {
		r.recordEvalOutcome(ctx, ruleID, ruleVersion, ruleType, false, 0, 0, EvalFailureQuarantined, qErr)
		return false, qErr
	}

	timeout := r.EvalTimeout()
	evalCtx, cancel := context.WithCancel(WithDBTimeTracker(context.WithoutCancel(ctx)))
	defer cancel()

	snapshots := r.snapshotRuleStates(ruleID)
	start := time.Now()

	var (
		res             evalCallResult
		timedOutByTimer bool
	)
	if timeout > 0 {
		done := make(chan evalCallResult, 1)
		go func() {
			var out evalCallResult
			defer func() {
				if rec := recover(); rec != nil {
					out.panicked = true
					out.panicVal = rec
				}
				done <- out
			}()
			out.matched, out.err = r.evaluate(evalCtx, ruleType, ev, params)
		}()

		waitDur := timeout
		for {
			timer := time.NewTimer(waitDur)
			select {
			case res = <-done:
				timer.Stop()
			case <-timer.C:
				dbSoFar, _ := AttributedDBDuration(evalCtx)
				ruleSoFar := time.Since(start) - dbSoFar
				if ruleSoFar < timeout {
					waitDur = timeout - ruleSoFar
					if waitDur < time.Millisecond {
						waitDur = time.Millisecond
					}
					continue
				}
				timedOutByTimer = true
				cancel()
				select {
				case res = <-done:
				case <-time.After(2 * time.Millisecond):
					res = evalCallResult{err: context.DeadlineExceeded}
				}
			}
			break
		}
	} else {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					res.panicked = true
					res.panicVal = rec
				}
			}()
			res.matched, res.err = r.evaluate(evalCtx, ruleType, ev, params)
		}()
	}

	totalDuration := time.Since(start)
	dbDuration, hadStorageErr := AttributedDBDuration(evalCtx)
	ruleDuration := totalDuration - dbDuration
	if ruleDuration < 0 {
		ruleDuration = 0
	}

	if res.panicked {
		r.restoreRuleStates(ruleID, snapshots)
		fErr := &RuleEvalError{
			RuleID:     ruleID,
			RuleType:   ruleType,
			Kind:       EvalFailurePanic,
			Duration:   ruleDuration,
			DBDuration: dbDuration,
			PanicValue: res.panicVal,
			Cause:      ErrRulePanic,
		}
		fErr.Quarantined = r.recordEvalOutcome(ctx, ruleID, ruleVersion, ruleType, false, ruleDuration, dbDuration, EvalFailurePanic, fErr)
		return false, fErr
	}

	if hadStorageErr || IsStorageError(res.err) {
		if res.err != nil {
			r.restoreRuleStates(ruleID, snapshots)
			sErr := &RuleEvalError{
				RuleID:     ruleID,
				RuleType:   ruleType,
				Kind:       EvalFailureStorage,
				Duration:   ruleDuration,
				DBDuration: dbDuration,
				Cause:      res.err,
			}
			r.recordEvalOutcome(ctx, ruleID, ruleVersion, ruleType, false, ruleDuration, dbDuration, EvalFailureStorage, sErr)
			return false, sErr
		}
		r.recordEvalOutcome(ctx, ruleID, ruleVersion, ruleType, res.matched, ruleDuration, dbDuration, EvalFailureStorage, nil)
		return res.matched, nil
	}

	timedOut := timedOutByTimer || errors.Is(res.err, context.DeadlineExceeded) || (timeout > 0 && ruleDuration >= timeout)
	if timedOut {
		// If the deadline was exceeded only because database I/O consumed the
		// budget while pure rule time stayed within timeout, do not blame or
		// quarantine the rule.
		if timeout > 0 && dbDuration > 0 && ruleDuration < timeout {
			r.recordEvalOutcome(ctx, ruleID, ruleVersion, ruleType, res.matched, ruleDuration, dbDuration, EvalFailureNone, nil)
			if errors.Is(res.err, context.DeadlineExceeded) {
				r.restoreRuleStates(ruleID, snapshots)
				return false, WrapStorageError(res.err)
			}
			return res.matched, res.err
		}
		r.restoreRuleStates(ruleID, snapshots)
		fErr := &RuleEvalError{
			RuleID:     ruleID,
			RuleType:   ruleType,
			Kind:       EvalFailureTimeout,
			Duration:   ruleDuration,
			DBDuration: dbDuration,
			Cause:      ErrRuleTimeout,
		}
		fErr.Quarantined = r.recordEvalOutcome(ctx, ruleID, ruleVersion, ruleType, false, ruleDuration, dbDuration, EvalFailureTimeout, fErr)
		return false, fErr
	}

	if res.err != nil {
		r.restoreRuleStates(ruleID, snapshots)
		fErr := &RuleEvalError{
			RuleID:     ruleID,
			RuleType:   ruleType,
			Kind:       EvalFailureError,
			Duration:   ruleDuration,
			DBDuration: dbDuration,
			Cause:      res.err,
		}
		fErr.Quarantined = r.recordEvalOutcome(ctx, ruleID, ruleVersion, ruleType, false, ruleDuration, dbDuration, EvalFailureError, fErr)
		return false, fErr
	}

	r.recordEvalOutcome(ctx, ruleID, ruleVersion, ruleType, res.matched, ruleDuration, dbDuration, EvalFailureNone, nil)
	return res.matched, nil
}
