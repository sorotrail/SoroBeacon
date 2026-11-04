package rules

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

type slowOrPanicEvaluator struct {
	delay     time.Duration
	panicMsg  string
	err       error
	dbDelay   time.Duration
	dbErr     error
	callCount atomic.Int64
}

func (e *slowOrPanicEvaluator) Validate(json.RawMessage) error { return nil }

func (e *slowOrPanicEvaluator) Evaluate(ctx context.Context, _ *stellar.DecodedEvent, _ json.RawMessage) (bool, error) {
	e.callCount.Add(1)
	if e.dbDelay > 0 || e.dbErr != nil {
		done := EnterDB(ctx)
		if e.dbDelay > 0 {
			time.Sleep(e.dbDelay)
		}
		done(e.dbErr)
		if e.dbErr != nil {
			return false, WrapStorageError(e.dbErr)
		}
	}
	if e.panicMsg != "" {
		panic(e.panicMsg)
	}
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if e.err != nil {
		return false, e.err
	}
	return true, nil
}

func TestRuleIsolationTimeoutAbortsSlowRuleAndQuarantinesAfterThreshold(t *testing.T) {
	reg := NewRegistry().
		WithEvalTimeout(20 * time.Millisecond).
		WithQuarantineThreshold(2)

	slow := &slowOrPanicEvaluator{delay: 150 * time.Millisecond}
	reg.Register("slow_test", slow)

	var transitions []RuleQuarantineState
	reg.WithQuarantineHook(func(_ context.Context, state RuleQuarantineState) {
		transitions = append(transitions, state)
	})

	ev := transferEvent(1)
	ctx := WithRuleVersion(WithRuleID(context.Background(), 42), 1)

	// First timeout: strike 1, not yet quarantined.
	matched, err := reg.Evaluate(ctx, "slow_test", ev, json.RawMessage(`{}`))
	require.Error(t, err)
	assert.False(t, matched)
	assert.ErrorIs(t, err, ErrRuleTimeout)
	assert.True(t, IsRuleIsolatedFailure(err))

	st, ok := reg.QuarantineState(42)
	require.True(t, ok)
	assert.Equal(t, 1, st.ConsecutiveFailures)
	assert.False(t, st.Quarantined)

	// Healthy rule on the same registry still evaluates immediately.
	fastCtx := WithRuleID(context.Background(), 99)
	fastMatched, err := reg.Evaluate(fastCtx, TypeEventEmitted, ev, json.RawMessage(`{"event_name":"transfer"}`))
	require.NoError(t, err)
	assert.True(t, fastMatched)

	// Second timeout: reaches threshold 2 -> rule 42 is quarantined.
	_, err = reg.Evaluate(ctx, "slow_test", ev, json.RawMessage(`{}`))
	require.ErrorIs(t, err, ErrRuleTimeout)

	st, ok = reg.QuarantineState(42)
	require.True(t, ok)
	assert.True(t, st.Quarantined)
	assert.Equal(t, EvalFailureTimeout, st.Reason)
	assert.Equal(t, 1, reg.QuarantinedCount())
	require.Len(t, transitions, 1)
	assert.True(t, transitions[0].Quarantined)

	// Subsequent evaluation is skipped immediately without invoking the slow evaluator.
	callsBefore := slow.callCount.Load()
	_, err = reg.Evaluate(ctx, "slow_test", ev, json.RawMessage(`{}`))
	require.ErrorIs(t, err, ErrRuleQuarantined)
	assert.Equal(t, callsBefore, slow.callCount.Load(), "quarantined rule must not execute")

	// Updating the rule version clears quarantine automatically.
	slow.delay = 0
	ctxV2 := WithRuleVersion(WithRuleID(context.Background(), 42), 2)
	matched, err = reg.Evaluate(ctxV2, "slow_test", ev, json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.True(t, matched)
	assert.Equal(t, 0, reg.QuarantinedCount())
	require.Len(t, transitions, 2)
	assert.False(t, transitions[1].Quarantined)
}

func TestRuleIsolationPanicRecoveryAndExplicitReenable(t *testing.T) {
	reg := NewRegistry().
		WithEvalTimeout(50 * time.Millisecond).
		WithQuarantineThreshold(2)

	panicker := &slowOrPanicEvaluator{panicMsg: "boom in evaluator"}
	reg.Register("panic_test", panicker)

	ev := transferEvent(1)
	ctx := WithRuleID(context.Background(), 77)

	for i := 0; i < 2; i++ {
		matched, err := reg.Evaluate(ctx, "panic_test", ev, json.RawMessage(`{}`))
		require.Error(t, err)
		assert.False(t, matched)
		assert.ErrorIs(t, err, ErrRulePanic)
		assert.True(t, IsRuleIsolatedFailure(err))
	}

	st, ok := reg.QuarantineState(77)
	require.True(t, ok)
	assert.True(t, st.Quarantined)
	assert.Equal(t, EvalFailurePanic, st.Reason)
	assert.EqualValues(t, 2, st.TotalPanics)

	// Operator fixes the evaluator and re-enables the rule.
	panicker.panicMsg = ""
	require.True(t, reg.ReenableRule(context.Background(), 77))
	assert.Equal(t, 0, reg.QuarantinedCount())

	matched, err := reg.Evaluate(ctx, "panic_test", ev, json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.True(t, matched)
}

func TestRuleIsolationDBCostExcludedFromRuleTimeoutAndQuarantine(t *testing.T) {
	reg := NewRegistry().
		WithEvalTimeout(25 * time.Millisecond).
		WithQuarantineThreshold(1)

	// DB takes 60ms (> 25ms rule timeout), but pure rule computation is sub-millisecond.
	dbBound := &slowOrPanicEvaluator{dbDelay: 60 * time.Millisecond}
	reg.Register("db_bound", dbBound)

	var lastObs EvalObservation
	reg.WithEvalObserver(func(_ context.Context, obs EvalObservation) {
		lastObs = obs
	})

	ctx := WithRuleID(context.Background(), 101)
	matched, err := reg.Evaluate(ctx, "db_bound", transferEvent(1), json.RawMessage(`{}`))
	require.NoError(t, err, "slow DB I/O must not trigger a rule timeout")
	assert.True(t, matched)
	assert.Equal(t, EvalFailureNone, lastObs.FailureKind)
	assert.GreaterOrEqual(t, lastObs.DBDuration, 50*time.Millisecond)
	assert.Less(t, lastObs.RuleDuration, 25*time.Millisecond)
	assert.Equal(t, 0, reg.QuarantinedCount())

	// A storage error must not quarantine the rule and must not be classified as an isolated rule failure.
	dbBound.dbDelay = 0
	dbBound.dbErr = errors.New("connection reset by peer")
	_, err = reg.Evaluate(ctx, "db_bound", transferEvent(1), json.RawMessage(`{}`))
	require.Error(t, err)
	assert.False(t, IsRuleIsolatedFailure(err), "storage failure must hold cursor rather than be treated as rule failure")
	assert.Equal(t, 0, reg.QuarantinedCount())
}

func TestCompositeFailureRollsBackStatefulChildAtomically(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	reg := NewRegistry().
		WithEvalTimeout(50 * time.Millisecond).
		WithQuarantineThreshold(5)

	freq := NewFrequencyThreshold().WithClock(clk.now)
	reg.Register(TypeFrequencyThreshold, freq)

	failingChild := &slowOrPanicEvaluator{panicMsg: "second child exploded"}
	reg.Register("exploding_child", failingChild)

	compositeParams := json.RawMessage(`{
		"op": "and",
		"rules": [
			{"type": "frequency_threshold", "params": {"event_name": "transfer", "count": 1, "window": "5m"}},
			{"type": "exploding_child", "params": {}}
		]
	}`)

	ctx := WithRuleID(context.Background(), 501)
	ev := transferEvent(1)

	// Child 1 (frequency_threshold with count=1) runs first and would arm an
	// episode if not rolled back when Child 2 panics.
	clk.advance(time.Second)
	matched, err := reg.Evaluate(ctx, TypeComposite, ev, compositeParams)
	require.ErrorIs(t, err, ErrRulePanic)
	assert.False(t, matched)
	assert.Empty(t, freq.AlertEventID(ctx, ev, nil), "failed composite evaluation must roll back frequency child state")

	// Once Child 2 stops panicking, the next event inside the same 5m window
	// must fire cleanly because the aborted evaluation did not leave the
	// frequency child armed.
	failingChild.panicMsg = ""
	clk.advance(time.Second)
	matched, err = reg.Evaluate(ctx, TypeComposite, ev, compositeParams)
	require.NoError(t, err)
	assert.True(t, matched, "frequency child must fire after rollback once sibling child succeeds")
}

func TestFrequencyThresholdSeedDoesNotBlockOtherRulesAndBoundsMemory(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	slowSeedStarted := make(chan struct{})
	releaseSlowSeed := make(chan struct{})

	f := NewFrequencyThreshold().WithClock(clk.now).WithMatchLog(
		MatchLogFunc(func(_ context.Context, ruleID int64, _ time.Time) ([]MatchRecord, error) {
			if ruleID == 1 {
				close(slowSeedStarted)
				<-releaseSlowSeed
			}
			return nil, nil
		}),
	)

	ev := transferEvent(1)
	params := json.RawMessage(`{"event_name":"transfer","count":1,"window":"5m"}`)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := f.Evaluate(WithRuleID(context.Background(), 1), ev, params)
		assert.NoError(t, err)
	}()

	<-slowSeedStarted

	// Rule 2 must evaluate without waiting for Rule 1's slow MatchLog query.
	doneRule2 := make(chan struct{})
	go func() {
		defer close(doneRule2)
		matched, err := f.Evaluate(WithRuleID(context.Background(), 2), ev, params)
		require.NoError(t, err)
		assert.True(t, matched)
	}()

	select {
	case <-doneRule2:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("rule 2 was blocked by rule 1's MatchLog query")
	}

	close(releaseSlowSeed)
	wg.Wait()
}
