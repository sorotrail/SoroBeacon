package poller

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAdjustIntervalDisabledLeavesCurrent(t *testing.T) {
	cur := 5 * time.Second
	assert.Equal(t, cur, AdjustInterval(cur, 0, 0, true))
	assert.Equal(t, cur, AdjustInterval(cur, 0, 30*time.Second, true))
	assert.Equal(t, cur, AdjustInterval(cur, time.Second, 0, false))
}

func TestAdjustIntervalBacklogHalvesTowardMin(t *testing.T) {
	min, max := time.Second, 30*time.Second
	got := AdjustInterval(8*time.Second, min, max, true)
	assert.Equal(t, 4*time.Second, got)
	got = AdjustInterval(got, min, max, true)
	assert.Equal(t, 2*time.Second, got)
	got = AdjustInterval(got, min, max, true)
	assert.Equal(t, time.Second, got)
	got = AdjustInterval(got, min, max, true)
	assert.Equal(t, time.Second, got, "must not drop below min")
}

func TestAdjustIntervalIdleDoublesTowardMax(t *testing.T) {
	min, max := time.Second, 16*time.Second
	got := AdjustInterval(time.Second, min, max, false)
	assert.Equal(t, 2*time.Second, got)
	got = AdjustInterval(got, min, max, false)
	assert.Equal(t, 4*time.Second, got)
	got = AdjustInterval(got, min, max, false)
	assert.Equal(t, 8*time.Second, got)
	got = AdjustInterval(got, min, max, false)
	assert.Equal(t, 16*time.Second, got)
	got = AdjustInterval(got, min, max, false)
	assert.Equal(t, 16*time.Second, got, "must not exceed max")
}

func TestAdjustIntervalNeverBelowHardFloor(t *testing.T) {
	// A misconfigured min below 1s is still floored.
	got := AdjustInterval(2*time.Second, 100*time.Millisecond, time.Second, true)
	assert.Equal(t, time.Second, got)
}

func TestClampInterval(t *testing.T) {
	min, max := 2*time.Second, 10*time.Second
	assert.Equal(t, 2*time.Second, clampInterval(time.Second, min, max))
	assert.Equal(t, 10*time.Second, clampInterval(30*time.Second, min, max))
	assert.Equal(t, 5*time.Second, clampInterval(5*time.Second, min, max))
	assert.Equal(t, 5*time.Second, clampInterval(5*time.Second, 0, 0))
}

func TestPollerEffectiveIntervalTracksSyntheticCycles(t *testing.T) {
	p := New(nil, nil, nil, nil, 8*time.Second, slog.New(slog.DiscardHandler)).
		WithAdaptive(time.Second, 32*time.Second)
	assert.Equal(t, 8*time.Second, p.EffectiveInterval())

	apply := func(backlog bool) {
		next := AdjustInterval(p.EffectiveInterval(), p.min, p.max, backlog)
		p.effective.Store(int64(next))
	}
	apply(true)
	assert.Equal(t, 4*time.Second, p.EffectiveInterval())
	apply(false)
	assert.Equal(t, 8*time.Second, p.EffectiveInterval())
	apply(false)
	assert.Equal(t, 16*time.Second, p.EffectiveInterval())
}

func TestWithAdaptiveClampsStart(t *testing.T) {
	p := New(nil, nil, nil, nil, 60*time.Second, slog.New(slog.DiscardHandler)).
		WithAdaptive(2*time.Second, 10*time.Second)
	assert.Equal(t, 10*time.Second, p.EffectiveInterval())
}
