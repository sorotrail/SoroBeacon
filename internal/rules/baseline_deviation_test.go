package rules

import (
	"context"
	"encoding/json"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func baselineTransferEvent(amount int64) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:         "evt-baseline",
		ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4",
		Topics:     []any{"transfer"},
		Value:      map[string]any{"amount": big.NewInt(amount)},
	}
}

func TestBaselineDeviation_StableBaselineNoAlertAndGenuineDeviation(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	bd := NewBaselineDeviation().WithClock(func() time.Time { return now })
	ctx := WithRuleID(context.Background(), 101)
	params := json.RawMessage(`{
		"event_name": "transfer",
		"value_path": "amount",
		"window": "1h",
		"warmup": "0s",
		"min_samples": 5,
		"sensitivity": 3,
		"direction": "both",
		"seasonality": "none"
	}`)

	// Feed 5 stable samples around 100.
	for _, v := range []int64{98, 100, 102, 99, 101} {
		now = now.Add(time.Minute)
		matched, err := bd.Evaluate(ctx, baselineTransferEvent(v), params)
		require.NoError(t, err)
		assert.False(t, matched, "warm-up sample %d must not fire", v)
	}

	// Normal observation within baseline spread does not fire.
	now = now.Add(time.Minute)
	matched, err := bd.Evaluate(ctx, baselineTransferEvent(103), params)
	require.NoError(t, err)
	assert.False(t, matched)
	dec, ok := bd.LastDecision(101)
	require.True(t, ok)
	assert.False(t, dec.Matched)
	assert.False(t, dec.WarmupActive)
	assert.InDelta(t, 100.0, dec.Baseline, 2.0)

	// Genuine upward spike (250 against ~100 baseline) fires and explains why.
	now = now.Add(time.Minute)
	expl, err := bd.Explain(ctx, baselineTransferEvent(250), params)
	require.NoError(t, err)
	assert.True(t, expl.Matched)
	require.NotEmpty(t, expl.Conditions)

	matched, err = bd.Evaluate(ctx, baselineTransferEvent(250), params)
	require.NoError(t, err)
	assert.True(t, matched)
	dec, ok = bd.LastDecision(101)
	require.True(t, ok)
	assert.True(t, dec.Matched)
	assert.Equal(t, 250.0, dec.Observed)
	assert.GreaterOrEqual(t, dec.Deviation, 3.0)
	assert.Contains(t, dec.Reason, "deviates by")

	// Direction filter: "above" ignores downward drops while "below" catches them.
	aboveParams := json.RawMessage(`{
		"event_name": "transfer",
		"value_path": "amount",
		"window": "1h",
		"warmup": "0s",
		"min_samples": 5,
		"sensitivity": 3,
		"direction": "above"
	}`)
	ctxAbove := WithRuleID(context.Background(), 102)
	for _, v := range []int64{98, 100, 102, 99, 101} {
		now = now.Add(time.Minute)
		_, err := bd.Evaluate(ctxAbove, baselineTransferEvent(v), aboveParams)
		require.NoError(t, err)
	}
	matched, err = bd.Evaluate(ctxAbove, baselineTransferEvent(10), aboveParams)
	require.NoError(t, err)
	assert.False(t, matched, "direction=above must not fire on a downward drop")
}

func TestBaselineDeviation_WarmupSuppressesAlertingWithVisibleRemainingDuration(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	bd := NewBaselineDeviation().WithClock(func() time.Time { return now })
	ctx := WithRuleID(context.Background(), 201)
	params := json.RawMessage(`{
		"event_name": "transfer",
		"value_path": "amount",
		"window": "1h",
		"warmup": "10m",
		"min_samples": 3,
		"sensitivity": 3
	}`)

	// Collect 4 samples in the first 3 minutes (min_samples=3 satisfied, but 10m warmup still active).
	for i := range 4 {
		if i > 0 {
			now = now.Add(time.Minute)
		}
		matched, err := bd.Evaluate(ctx, baselineTransferEvent(100), params)
		require.NoError(t, err)
		assert.False(t, matched)
	}

	// At t+4m (6m remaining in warmup), a massive spike is learned without firing.
	now = now.Add(time.Minute)
	matched, err := bd.Evaluate(ctx, baselineTransferEvent(100), params)
	require.NoError(t, err)
	assert.False(t, matched)
	dec, ok := bd.LastDecision(201)
	require.True(t, ok)
	assert.True(t, dec.WarmupActive)
	assert.Equal(t, 6*time.Minute, dec.WarmupRemaining)
	assert.Contains(t, dec.Reason, "6m0s remaining")

	// Advance past the 10-minute warm-up window; now a genuine spike fires.
	now = now.Add(7 * time.Minute)
	matched, err = bd.Evaluate(ctx, baselineTransferEvent(500), params)
	require.NoError(t, err)
	assert.True(t, matched)
	dec, ok = bd.LastDecision(201)
	require.True(t, ok)
	assert.False(t, dec.WarmupActive)
	assert.Zero(t, dec.WarmupRemaining)
}

func TestBaselineDeviation_DegenerateConstantAndSingleSpikeHistory(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	bd := NewBaselineDeviation().WithClock(func() time.Time { return now })
	params := json.RawMessage(`{
		"event_name": "transfer",
		"value_path": "amount",
		"window": "1h",
		"warmup": "0s",
		"min_samples": 5,
		"sensitivity": 3
	}`)

	// 1. Constant history [100, 100, 100, 100, 100]: must not divide by zero or fire on 100.
	ctxConst := WithRuleID(context.Background(), 301)
	for range 5 {
		now = now.Add(time.Minute)
		matched, err := bd.Evaluate(ctxConst, baselineTransferEvent(100), params)
		require.NoError(t, err)
		assert.False(t, matched)
	}
	now = now.Add(time.Minute)
	matched, err := bd.Evaluate(ctxConst, baselineTransferEvent(100), params)
	require.NoError(t, err)
	assert.False(t, matched, "identical value on constant baseline must not fire")

	now = now.Add(time.Minute)
	matched, err = bd.Evaluate(ctxConst, baselineTransferEvent(500), params)
	require.NoError(t, err)
	assert.True(t, matched, "4x jump above constant baseline must fire cleanly")

	// 2. Single huge spike during warm-up [99, 100, 101, 98, 100000, 102]:
	// median + MAD resists the single outlier so normal values stay quiet and real anomalies fire.
	ctxSpike := WithRuleID(context.Background(), 302)
	for _, v := range []int64{99, 100, 101, 98, 100000, 102} {
		now = now.Add(time.Minute)
		_, err := bd.Evaluate(ctxSpike, baselineTransferEvent(v), params)
		require.NoError(t, err)
	}
	now = now.Add(time.Minute)
	matched, err = bd.Evaluate(ctxSpike, baselineTransferEvent(103), params)
	require.NoError(t, err)
	assert.False(t, matched, "single warm-up spike must not cause normal traffic to misfire")

	now = now.Add(time.Minute)
	matched, err = bd.Evaluate(ctxSpike, baselineTransferEvent(200), params)
	require.NoError(t, err)
	assert.True(t, matched, "single warm-up spike must not mask a genuine 200 anomaly")
}

func TestBaselineDeviation_MaintenanceWindowDoesNotPolluteBaseline(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	inMaintenance := false
	bd := NewBaselineDeviation().
		WithClock(func() time.Time { return now }).
		WithSuppressionCheck(func(_ context.Context, _ *stellar.DecodedEvent) bool {
			return inMaintenance
		})
	ctx := WithRuleID(context.Background(), 401)
	params := json.RawMessage(`{
		"event_name": "transfer",
		"value_path": "amount",
		"window": "1h",
		"warmup": "0s",
		"min_samples": 5,
		"sensitivity": 3
	}`)

	for _, v := range []int64{98, 100, 102, 99, 101} {
		now = now.Add(time.Minute)
		matched, err := bd.Evaluate(ctx, baselineTransferEvent(v), params)
		require.NoError(t, err)
		assert.False(t, matched)
	}

	// During maintenance window (both via hook and WithBaselineSuppressed), send 15 huge spikes.
	inMaintenance = true
	for range 10 {
		now = now.Add(time.Minute)
		matched, err := bd.Evaluate(ctx, baselineTransferEvent(50000), params)
		require.NoError(t, err)
		assert.False(t, matched, "maintenance window traffic must not fire")
	}
	inMaintenance = false
	suppressedCtx := WithBaselineSuppressed(ctx, true)
	for range 5 {
		now = now.Add(time.Minute)
		matched, err := bd.Evaluate(suppressedCtx, baselineTransferEvent(50000), params)
		require.NoError(t, err)
		assert.False(t, matched, "context-suppressed traffic must not fire")
	}

	// After maintenance ends, the baseline is still ~100 (unpolluted by the 50,000 spikes).
	now = now.Add(time.Minute)
	matched, err := bd.Evaluate(ctx, baselineTransferEvent(250), params)
	require.NoError(t, err)
	assert.True(t, matched, "post-maintenance anomaly must still fire against unpolluted ~100 baseline")
	dec, ok := bd.LastDecision(401)
	require.True(t, ok)
	assert.InDelta(t, 100.0, dec.Baseline, 2.0)
}

func TestBaselineDeviation_SeasonalityHourOfDayBuckets(t *testing.T) {
	now := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	bd := NewBaselineDeviation().WithClock(func() time.Time { return now })
	ctx := WithRuleID(context.Background(), 501)
	params := json.RawMessage(`{
		"event_name": "transfer",
		"value_path": "amount",
		"window": "168h",
		"warmup": "0s",
		"min_samples": 5,
		"sensitivity": 3,
		"direction": "above",
		"seasonality": "hour_of_day"
	}`)

	// Seed 09:00 UTC bucket with busy daytime volume (~1000).
	for _, v := range []int64{980, 1000, 1020, 990, 1010} {
		now = now.Add(time.Minute)
		_, err := bd.Evaluate(ctx, baselineTransferEvent(v), params)
		require.NoError(t, err)
	}

	// Seed 02:00 UTC bucket with quiet overnight volume (~50).
	now = time.Date(2026, 6, 2, 2, 0, 0, 0, time.UTC)
	for _, v := range []int64{48, 50, 52, 49, 51} {
		now = now.Add(time.Minute)
		_, err := bd.Evaluate(ctx, baselineTransferEvent(v), params)
		require.NoError(t, err)
	}

	// Value of 250 at 09:10 UTC is below the 09:00 baseline (1000) -> does not fire.
	now = time.Date(2026, 6, 2, 9, 10, 0, 0, time.UTC)
	matched, err := bd.Evaluate(ctx, baselineTransferEvent(250), params)
	require.NoError(t, err)
	assert.False(t, matched)

	// Same value of 250 at 02:10 UTC is a 5x spike over the 02:00 baseline (50) -> fires!
	now = time.Date(2026, 6, 3, 2, 10, 0, 0, time.UTC)
	matched, err = bd.Evaluate(ctx, baselineTransferEvent(250), params)
	require.NoError(t, err)
	assert.True(t, matched)
	dec, ok := bd.LastDecision(501)
	require.True(t, ok)
	assert.Equal(t, "hour:02", dec.SeasonalityBucket)
}

func TestBaselineDeviation_ValidationAndConcurrency(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Validate(TypeBaselineDeviation, json.RawMessage(`{"window":"1h","sensitivity":3}`)))
	require.Error(t, r.Validate(TypeBaselineDeviation, json.RawMessage(`{"sensitivity":3}`)))
	require.Error(t, r.Validate(TypeBaselineDeviation, json.RawMessage(`{"window":"1h","sensitivity":0}`)))
	require.Error(t, r.Validate(TypeBaselineDeviation, json.RawMessage(`{"window":"1h","sensitivity":3,"direction":"sideways"}`)))
	require.Error(t, r.Validate(TypeBaselineDeviation, json.RawMessage(`{"window":"1h","sensitivity":3,"seasonality":"monthly"}`)))

	params := json.RawMessage(`{"event_name":"transfer","value_path":"amount","window":"1h","warmup":"0s","min_samples":3,"sensitivity":3}`)
	ctx := WithRuleID(context.Background(), 601)
	var wg sync.WaitGroup
	wg.Add(8)
	for i := range 8 {
		go func(id int) {
			defer wg.Done()
			for j := range 20 {
				_, err := r.Evaluate(ctx, TypeBaselineDeviation, baselineTransferEvent(int64(100+id+j)), params)
				assert.NoError(t, err)
			}
		}(i)
	}
	wg.Wait()
}
