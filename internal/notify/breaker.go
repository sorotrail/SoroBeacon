package notify

import (
	"sync"
	"time"
)

// BreakerState represents the state of a channel circuit breaker.
type BreakerState string

const (
	StateClosed   BreakerState = "closed"
	StateOpen     BreakerState = "open"
	StateHalfOpen BreakerState = "half-open"
)

// ChannelBreaker manages the circuit breaker state for a single channel.
type ChannelBreaker struct {
	mu               sync.Mutex
	state            BreakerState
	failures         int
	consecutiveLimit int
	cooldown         time.Duration
	lastStateChange  time.Time
}

// NewChannelBreaker creates a circuit breaker for a channel with the given consecutive failure threshold and recovery cooldown.
func NewChannelBreaker(consecutiveLimit int, cooldown time.Duration) *ChannelBreaker {
	if consecutiveLimit <= 0 {
		consecutiveLimit = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &ChannelBreaker{
		state:            StateClosed,
		consecutiveLimit: consecutiveLimit,
		cooldown:         cooldown,
		lastStateChange:  time.Now(),
	}
}

// State returns the current state of the breaker, taking time-based transition to half-open into account.
func (cb *ChannelBreaker) State() BreakerState {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateOpen && time.Since(cb.lastStateChange) >= cb.cooldown {
		cb.state = StateHalfOpen
		cb.lastStateChange = time.Now()
	}
	return cb.state
}

// Allow checks whether an operation is allowed to proceed. When in half-open state, it permits a probe.
func (cb *ChannelBreaker) Allow() bool {
	s := cb.State()
	return s == StateClosed || s == StateHalfOpen
}

// RecordSuccess records a successful delivery, resetting the breaker to closed state.
func (cb *ChannelBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.state = StateClosed
	cb.failures = 0
	cb.lastStateChange = time.Now()
}

// RecordFailure records a delivery failure, incrementing consecutive failures and opening the breaker if the threshold is reached.
func (cb *ChannelBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		cb.failures++
		if cb.failures >= cb.consecutiveLimit {
			cb.state = StateOpen
			cb.lastStateChange = time.Now()
		}
	case StateHalfOpen:
		// A failure during half-open immediately returns the breaker to open state.
		cb.state = StateOpen
		cb.failures = cb.consecutiveLimit
		cb.lastStateChange = time.Now()
	case StateOpen:
		// Already open, refresh last state change or keep it.
	}
}

// BreakerRegistry manages circuit breakers keyed by channel ID in a thread-safe manner.
type BreakerRegistry struct {
	mu               sync.Mutex
	breakers         map[int64]*ChannelBreaker
	consecutiveLimit int
	cooldown         time.Duration
}

// NewBreakerRegistry initializes a registry for per-channel circuit breakers.
func NewBreakerRegistry(consecutiveLimit int, cooldown time.Duration) *BreakerRegistry {
	return &BreakerRegistry{
		breakers:         make(map[int64]*ChannelBreaker),
		consecutiveLimit: consecutiveLimit,
		cooldown:         cooldown,
	}
}

// Get returns the circuit breaker for the specified channel ID, creating one if it does not exist.
func (r *BreakerRegistry) Get(channelID int64) *ChannelBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()

	cb, exists := r.breakers[channelID]
	if !exists {
		cb = NewChannelBreaker(r.consecutiveLimit, r.cooldown)
		r.breakers[channelID] = cb
	}
	return cb
}
