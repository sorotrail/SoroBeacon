package store

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreBreaker_TransitionsAndBackoff(t *testing.T) {
	now := time.Now()
	b := NewStoreBreaker(3, 5*time.Second)
	b.WithNow(func() time.Time { return now })
	b.WithRand(func() float64 { return 0.5 }) // deterministic jitter

	require.Equal(t, StateClosed, b.State())
	require.False(t, b.Degraded())
	require.True(t, b.Allow())

	// Non-unavailable errors do not increment failure counter
	b.RecordFailure(errors.New("constraint error"))
	require.Equal(t, StateClosed, b.State())

	// 2 unavailable failures: still closed
	b.RecordFailure(ErrUnavailable)
	b.RecordFailure(ErrUnavailable)
	require.Equal(t, StateClosed, b.State())
	require.True(t, b.Allow())

	// 3rd unavailable failure trips the breaker to open
	b.RecordFailure(ErrUnavailable)
	require.Equal(t, StateOpen, b.State())
	require.True(t, b.Degraded())
	require.False(t, b.Allow())

	// Advance time before cooldown expires: still open
	now = now.Add(2 * time.Second)
	require.Equal(t, StateOpen, b.State())
	require.False(t, b.Allow())
	assert.True(t, b.RemainingCooldown() > 0)

	// Advance time past cooldown (base 5s with 0.5 jitter is 3.75s or similar):
	now = now.Add(10 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())
	require.True(t, b.Degraded())

	// In half-open, Allow() permits exactly one probe in flight
	require.True(t, b.Allow())
	require.False(t, b.Allow(), "second probe while in-flight is denied")

	// Probe fails -> trips open again with exponential backoff
	b.RecordFailure(ErrUnavailable)
	require.Equal(t, StateOpen, b.State())
	require.False(t, b.Allow())

	// Advance time past new backoff cooldown
	now = now.Add(20 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())
	require.True(t, b.Allow())

	// Probe succeeds -> breaker closes and resets
	b.RecordSuccess()
	require.Equal(t, StateClosed, b.State())
	require.False(t, b.Degraded())
	require.True(t, b.Allow())
}
