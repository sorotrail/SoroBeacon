package rules

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock lets the tests move time without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestFrequencyThresholdValidate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		wantErr bool
		field   string
	}{
		{"valid", `{"count": 5, "window": "5m"}`, false, ""},
		{"event name optional", `{"event_name": "transfer", "count": 5, "window": "5m"}`, false, ""},
		{"zero count", `{"count": 0, "window": "5m"}`, true, "count"},
		{"negative count", `{"count": -1, "window": "5m"}`, true, "count"},
		{"missing count", `{"window": "5m"}`, true, "count"},
		{"missing window", `{"count": 5}`, true, "window"},
		{"unparseable window", `{"count": 5, "window": "five minutes"}`, true, "window"},
		{"non-positive window", `{"count": 5, "window": "0s"}`, true, "window"},
		{"negative window", `{"count": 5, "window": "-1m"}`, true, "window"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewFrequencyThreshold().Validate(json.RawMessage(tt.params))
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var details FieldErrors
			require.ErrorAs(t, err, &details)
			fields := make([]string, 0, len(details))
			for _, d := range details {
				fields = append(fields, d.Field)
			}
			assert.Contains(t, fields, tt.field, "want a %q field error", tt.field)
		})
	}
}

type freqStep struct {
	advance time.Duration
	want    bool
}

// TestFrequencyThresholdCrossingAndWindow drives one rule through a sequence
// of events, advancing a fake clock, and checks when it fires. It covers the
// three "done" criteria that are about time: exactly one alert per crossing,
// sustained overage staying quiet, and window expiry dropping old matches.
func TestFrequencyThresholdCrossingAndWindow(t *testing.T) {
	tests := []struct {
		name   string
		params string
		steps  []freqStep
	}{
		{
			name:   "fires exactly on the crossing and not per event",
			params: `{"event_name": "transfer", "count": 3, "window": "5m"}`,
			steps: []freqStep{
				{0, false}, {time.Second, false}, {time.Second, true}, {time.Second, false},
			},
		},
		{
			name:   "sustained overage stays quiet inside the window",
			params: `{"event_name": "transfer", "count": 3, "window": "5m"}`,
			steps: []freqStep{
				{0, false}, {time.Second, false}, {time.Second, true},
				{time.Minute, false}, {time.Minute, false}, {2 * time.Minute, false},
			},
		},
		{
			name:   "window expiry drops old matches so a slow trickle never crosses",
			params: `{"event_name": "transfer", "count": 3, "window": "1m"}`,
			steps: []freqStep{
				{0, false}, {40 * time.Second, false}, {40 * time.Second, false},
				{40 * time.Second, false}, {40 * time.Second, false},
			},
		},
		{
			name:   "re-arms one window after firing",
			params: `{"event_name": "transfer", "count": 3, "window": "1m"}`,
			steps: []freqStep{
				{0, false}, {time.Second, false}, {time.Second, true},
				{28 * time.Second, false}, // t=30s, still inside the episode window
				{31 * time.Second, true},  // t=61s, past episode+1m: a new crossing
			},
		},
		{
			name:   "a different event name never counts",
			params: `{"event_name": "mint", "count": 1, "window": "5m"}`,
			steps: []freqStep{
				{0, false}, {time.Second, false},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			f := NewFrequencyThreshold().WithClock(clk.now)
			ctx := WithRuleID(context.Background(), 1)
			for i, step := range tt.steps {
				clk.advance(step.advance)
				got, err := f.Evaluate(ctx, transferEvent(1), json.RawMessage(tt.params))
				require.NoError(t, err, "step %d", i)
				assert.Equal(t, step.want, got, "step %d at %s", i, clk.t.UTC())
			}
		})
	}
}

// TestFrequencyThresholdSyntheticEventID pins the dedup contract: while an
// episode is active the rule hands the store one stable id (the window start),
// so a replayed crossing is dropped rather than alerting twice.
func TestFrequencyThresholdSyntheticEventID(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	params := json.RawMessage(`{"count": 2, "window": "5m"}`)
	f := NewFrequencyThreshold().WithClock(clk.now)
	ctx := WithRuleID(context.Background(), 1)

	clk.advance(time.Second)
	got, err := f.Evaluate(ctx, transferEvent(1), params)
	require.NoError(t, err)
	require.False(t, got)

	clk.advance(time.Second)
	got, err = f.Evaluate(ctx, transferEvent(1), params)
	require.NoError(t, err)
	require.True(t, got)

	first := f.AlertEventID(ctx, transferEvent(1), params)
	require.NotEmpty(t, first)
	assert.Contains(t, first, frequencyEventPrefix)
	// Still the same episode: the id must not move as more matches arrive.
	clk.advance(time.Second)
	_, err = f.Evaluate(ctx, transferEvent(1), params)
	require.NoError(t, err)
	assert.Equal(t, first, f.AlertEventID(ctx, transferEvent(1), params))
}

// TestFrequencyThresholdRebuildsStateAfterRestart is the restart behaviour: a
// new process must not alert again for an episode the rule already fired, and
// must still fire once a genuinely new crossing happens.
func TestFrequencyThresholdRebuildsStateAfterRestart(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	params := json.RawMessage(`{"count": 3, "window": "5m"}`)
	ev := transferEvent(1)
	ctx := WithRuleID(context.Background(), 1)

	first := NewFrequencyThreshold().WithClock(clk.now)
	for i := 0; i < 3; i++ {
		clk.advance(time.Second)
		got, err := first.Evaluate(ctx, ev, params)
		require.NoError(t, err)
		assert.Equal(t, i == 2, got, "the third match is the crossing")
	}
	episode := first.AlertEventID(ctx, ev, params)
	require.NotEmpty(t, episode)
	// The store persisted the alert under the synthetic id; on restart the
	// rule reads it back.
	stored := []MatchRecord{{At: clk.t, EventID: episode}}

	restarted := NewFrequencyThreshold().WithClock(clk.now).WithMatchLog(
		MatchLogFunc(func(context.Context, int64, time.Time) ([]MatchRecord, error) { return stored, nil }))

	for i := 0; i < 5; i++ {
		clk.advance(time.Second)
		got, err := restarted.Evaluate(ctx, ev, params)
		require.NoError(t, err)
		assert.False(t, got, "a restarted rule must not duplicate the episode's alert")
	}
	assert.Equal(t, episode, restarted.AlertEventID(ctx, ev, params),
		"the rebuilt state must recover the same synthetic id")
}

// TestFrequencyThresholdSkipsWhenRebuildFails checks that a durable-state read
// failure does not silently reset the count into a spurious alert: the event is
// skipped and the next one retries the rebuild.
func TestFrequencyThresholdSkipsWhenRebuildFails(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	params := json.RawMessage(`{"count": 1, "window": "5m"}`)
	ev := transferEvent(1)
	ctx := WithRuleID(context.Background(), 1)

	calls := 0
	log := MatchLogFunc(func(context.Context, int64, time.Time) ([]MatchRecord, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("database is down")
		}
		return nil, nil
	})
	f := NewFrequencyThreshold().WithClock(clk.now).WithMatchLog(log)

	clk.advance(time.Second)
	got, err := f.Evaluate(ctx, ev, params)
	require.NoError(t, err, "a failed rebuild is not an evaluation error")
	assert.False(t, got, "the crossing must not fire while the rebuild is pending")

	// The retry succeeds and the rule is free to fire on a real crossing.
	clk.advance(time.Second)
	got, err = f.Evaluate(ctx, ev, params)
	require.NoError(t, err)
	assert.True(t, got)
	assert.Equal(t, 2, calls)
}

func TestFrequencyThresholdEventNames(t *testing.T) {
	names, ok := NewFrequencyThreshold().EventNames(json.RawMessage(`{"event_name": "transfer", "count": 1, "window": "1m"}`))
	require.True(t, ok)
	assert.Equal(t, []string{"transfer"}, names)

	_, ok = NewFrequencyThreshold().EventNames(json.RawMessage(`{"count": 1, "window": "1m"}`))
	assert.False(t, ok, "a rule with no event_name matches every event and cannot be narrowed")
}

func TestRegistryValidatesFrequencyThreshold(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Validate(TypeFrequencyThreshold, json.RawMessage(`{"count": 5, "window": "5m"}`)))
	require.Error(t, r.Validate(TypeFrequencyThreshold, json.RawMessage(`{"count": 0, "window": "5m"}`)))
	require.Error(t, r.Validate(TypeFrequencyThreshold, json.RawMessage(`{"count": 5, "window": "soon"}`)))
	// The cross-cutting cooldown still applies on top.
	require.NoError(t, r.Validate(TypeFrequencyThreshold,
		json.RawMessage(`{"count": 5, "window": "5m", "cooldown": "1m"}`)))
}

func TestRuleIDContext(t *testing.T) {
	assert.Zero(t, RuleID(context.Background()))
	assert.EqualValues(t, 7, RuleID(WithRuleID(context.Background(), 7)))
}
