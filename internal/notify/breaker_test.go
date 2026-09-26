package notify

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestChannelBreaker_SuccessAndFailure(t *testing.T) {
	cb := NewChannelBreaker(2, 50*time.Millisecond)
	assert.Equal(t, StateClosed, cb.State())
	assert.True(t, cb.Allow())

	// First failure does not open breaker
	cb.RecordFailure()
	assert.Equal(t, StateClosed, cb.State())
	assert.True(t, cb.Allow())

	// Success resets failure count
	cb.RecordSuccess()
	assert.Equal(t, StateClosed, cb.State())

	// Reach failure limit
	cb.RecordFailure()
	cb.RecordFailure()
	assert.Equal(t, StateOpen, cb.State())
	assert.False(t, cb.Allow())
}

func TestChannelBreaker_HalfOpenAndRecovery(t *testing.T) {
	cooldown := 20 * time.Millisecond
	cb := NewChannelBreaker(1, cooldown)

	cb.RecordFailure()
	assert.Equal(t, StateOpen, cb.State())
	assert.False(t, cb.Allow())

	// Wait for cooldown to transition to half-open
	time.Sleep(cooldown + 5*time.Millisecond)
	assert.Equal(t, StateHalfOpen, cb.State())
	assert.True(t, cb.Allow()) // Probe allowed

	// Successful probe closes the breaker
	cb.RecordSuccess()
	assert.Equal(t, StateClosed, cb.State())
	assert.True(t, cb.Allow())
}

func TestChannelBreaker_HalfOpenFailureReopens(t *testing.T) {
	cooldown := 20 * time.Millisecond
	cb := NewChannelBreaker(1, cooldown)

	cb.RecordFailure()
	assert.Equal(t, StateOpen, cb.State())

	time.Sleep(cooldown + 5*time.Millisecond)
	assert.Equal(t, StateHalfOpen, cb.State())

	// Failed probe re-opens the breaker
	cb.RecordFailure()
	assert.Equal(t, StateOpen, cb.State())
	assert.False(t, cb.Allow())
}

func TestBreakerRegistry_Isolation(t *testing.T) {
	registry := NewBreakerRegistry(1, 100*time.Millisecond)

	cb1 := registry.Get(101)
	cb2 := registry.Get(102)

	// Open breaker for channel 101
	cb1.RecordFailure()
	assert.Equal(t, StateOpen, cb1.State())
	assert.False(t, cb1.Allow())

	// Channel 102 should remain unaffected and closed
	assert.Equal(t, StateClosed, cb2.State())
	assert.True(t, cb2.Allow())
}
