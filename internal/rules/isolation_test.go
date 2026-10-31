package rules

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

type stubEvaluator struct {
	evalFn func(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error)
}

func (s stubEvaluator) Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	return s.evalFn(ctx, ev, params)
}

func (stubEvaluator) Validate(json.RawMessage) error { return nil }

func TestRegistryIsolatesPanickingRuleAndQuarantinesAfterThreshold(t *testing.T) {
	reg := NewRegistry().WithQuarantineThreshold(3)
	var panicsObserved atomic.Int32
	reg.WithPanicObserver(func() { panicsObserved.Add(1) })

	var shouldPanic atomic.Bool
	shouldPanic.Store(true)
	reg.Register("custom_panic", stubEvaluator{
		evalFn: func(_ context.Context, _ *stellar.DecodedEvent, _ json.RawMessage) (bool, error) {
			if shouldPanic.Load() {
				panic("malformed contract payload")
			}
			return true, nil
		},
	})

	ev := transferEvent(100)
	badCtx := WithMonitorID(WithRuleID(context.Background(), 101), 10)
	goodCtx := WithMonitorID(WithRuleID(context.Background(), 102), 20)

	for i := 1; i <= 3; i++ {
		matched, err := reg.Evaluate(badCtx, "custom_panic", ev, json.RawMessage(`{}`))
		require.Error(t, err)
		assert.False(t, matched)
		assert.True(t, IsRulePanic(err), "panic must be recovered and typed")

		var panicErr *RulePanicError
		require.ErrorAs(t, err, &panicErr)
		assert.Equal(t, int64(101), panicErr.RuleID)
		assert.Equal(t, int64(10), panicErr.MonitorID)
		assert.Equal(t, "custom_panic", panicErr.RuleType)

		// Healthy rules in another workspace continue unaffected on every event.
		ok, err := reg.Evaluate(goodCtx, TypeEventEmitted, ev, json.RawMessage(`{"event_name":"transfer"}`))
		require.NoError(t, err)
		assert.True(t, ok)
	}

	assert.Equal(t, int32(3), panicsObserved.Load(), "panic observer must fire once per recovered panic")

	state, ok := reg.QuarantineState(101)
	require.True(t, ok)
	assert.True(t, state.Quarantined)
	assert.Equal(t, QuarantineReasonPanic, state.Reason)
	assert.Equal(t, 3, state.ConsecutiveFailures)
	assert.Equal(t, 3, state.TotalPanics)
	assert.Contains(t, state.LastError, "malformed contract payload")

	// Subsequent evaluations of the quarantined rule fail fast without invoking the evaluator.
	matched, err := reg.Evaluate(badCtx, "custom_panic", ev, json.RawMessage(`{}`))
	require.Error(t, err)
	assert.False(t, matched)
	assert.True(t, IsRuleQuarantined(err))
	assert.Equal(t, int32(3), panicsObserved.Load(), "quarantined rule must not execute and panic again")

	// Fix the rule and re-enable it from quarantine.
	shouldPanic.Store(false)
	require.True(t, reg.ReenableRule(101))
	stateAfter, ok := reg.QuarantineState(101)
	require.True(t, ok)
	assert.False(t, stateAfter.Quarantined)
	assert.Equal(t, 0, stateAfter.ConsecutiveFailures)

	matched, err = reg.Evaluate(badCtx, "custom_panic", ev, json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.True(t, matched)

	audit := reg.QuarantineAuditLog()
	require.Len(t, audit, 2)
	assert.Equal(t, "quarantined", audit[0].Action)
	assert.Equal(t, QuarantineReasonPanic, audit[0].Reason)
	assert.Equal(t, int64(101), audit[0].RuleID)
	assert.Equal(t, "reenabled", audit[1].Action)
}

func TestRegistryBoundsSlowRuleEvaluationAndQuarantinesRepeatedTimeouts(t *testing.T) {
	reg := NewRegistry().
		WithEvalTimeout(20 * time.Millisecond).
		WithQuarantineThreshold(2)

	reg.Register("slow_rule", stubEvaluator{
		evalFn: func(ctx context.Context, _ *stellar.DecodedEvent, _ json.RawMessage) (bool, error) {
			select {
			case <-time.After(250 * time.Millisecond):
				return true, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		},
	})

	ev := transferEvent(50)
	slowCtx := WithMonitorID(WithRuleID(context.Background(), 201), 1)
	fastCtx := WithMonitorID(WithRuleID(context.Background(), 202), 2)

	for i := 0; i < 2; i++ {
		start := time.Now()
		matched, err := reg.Evaluate(slowCtx, "slow_rule", ev, json.RawMessage(`{}`))
		elapsed := time.Since(start)
		require.ErrorIs(t, err, ErrRuleEvalTimeout)
		assert.False(t, matched)
		assert.Less(t, elapsed, 150*time.Millisecond, "slow rule must be cut off near its timeout budget")

		ok, err := reg.Evaluate(fastCtx, TypeEventEmitted, ev, json.RawMessage(`{"event_name":"transfer"}`))
		require.NoError(t, err)
		assert.True(t, ok)
	}

	st, ok := reg.QuarantineState(201)
	require.True(t, ok)
	assert.True(t, st.Quarantined)
	assert.Equal(t, QuarantineReasonTimeout, st.Reason)
	assert.Equal(t, 2, st.TotalTimeouts)
}

func TestDatabaseSlowdownDoesNotQuarantineStatefulRule(t *testing.T) {
	reg := NewRegistry().
		WithEvalTimeout(25 * time.Millisecond).
		WithQuarantineThreshold(2)

	slowMatchLog := MatchLogFunc(func(ctx context.Context, _ int64, _ time.Time) ([]MatchRecord, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	freq := NewFrequencyThreshold().WithMatchLog(slowMatchLog)
	reg.Register(TypeFrequencyThreshold, freq)

	ev := transferEvent(10)
	ruleCtx := WithMonitorID(WithRuleID(context.Background(), 301), 7)
	params := json.RawMessage(`{"event_name":"transfer","count":2,"window":"5m"}`)

	for i := 0; i < 4; i++ {
		_, err := reg.Evaluate(ruleCtx, TypeFrequencyThreshold, ev, params)
		require.ErrorIs(t, err, ErrRuleEvalTimeout)
		assert.True(t, IsDependencyAttributed(err), "timeout spent waiting on MatchLog must be dependency-attributed")
	}

	st, exists := reg.QuarantineState(301)
	assert.False(t, exists && st.Quarantined, "a rule that is slow because the database is slow must never be quarantined")
	assert.Equal(t, 0, freq.TrackedMatchCount(301), "aborted seed must not leave partial match state")
}

func TestFrequencyThresholdBoundsTrackedState(t *testing.T) {
	now := time.Date(2026, time.October, 27, 12, 0, 0, 0, time.UTC)
	freq := NewFrequencyThreshold().
		WithClock(func() time.Time {
			now = now.Add(time.Millisecond)
			return now
		}).
		WithMaxMatches(25)

	ctx := WithRuleID(context.Background(), 401)
	ev := transferEvent(10)
	params := json.RawMessage(`{"event_name":"transfer","count":100,"window":"1h"}`)

	for i := 0; i < 80; i++ {
		_, err := freq.Evaluate(ctx, ev, params)
		require.NoError(t, err)
	}

	assert.Equal(t, 25, freq.TrackedMatchCount(401), "in-memory state per rule must be capped at maxMatches")
}
