package store

import (
	"sync"
	"time"
)

// BreakerState represents the operating state of a store circuit breaker.
type BreakerState string

const (
	// StateClosed allows all requests through to the store.
	StateClosed BreakerState = "closed"
	// StateOpen fails fast without querying the database, protecting it from stampedes.
	StateOpen BreakerState = "open"
	// StateHalfOpen allows a bounded probe to test whether the store has recovered.
	StateHalfOpen BreakerState = "half-open"
)

// StoreBreaker manages circuit breaker protection in front of the database.
// When the store encounters consecutive unavailable failures, the breaker opens,
// causing subsequent requests to fail fast rather than queuing or hammering the database.
// Reconnection probes are scheduled with jittered exponential backoff to avoid stampeding.
type StoreBreaker struct {
	mu               sync.Mutex
	state            BreakerState
	consecutiveFails int
	consecutiveLimit int
	baseCooldown     time.Duration
	currentCooldown  time.Duration
	maxCooldown      time.Duration
	lastStateChange  time.Time
	now              func() time.Time
	randFloat        func() float64
	probeInFlight    bool
}

// NewStoreBreaker initializes a circuit breaker for store operations with consecutive
// failure threshold and initial cooldown duration before probing.
func NewStoreBreaker(consecutiveLimit int, cooldown time.Duration) *StoreBreaker {
	if consecutiveLimit <= 0 {
		consecutiveLimit = 3
	}
	if cooldown <= 0 {
		cooldown = 5 * time.Second
	}
	return &StoreBreaker{
		state:            StateClosed,
		consecutiveLimit: consecutiveLimit,
		baseCooldown:     cooldown,
		currentCooldown:  cooldown,
		maxCooldown:      60 * time.Second,
		lastStateChange:  time.Now(),
		now:              time.Now,
	}
}

// WithNow overrides the clock function, used in deterministic unit tests.
func (b *StoreBreaker) WithNow(fn func() time.Time) *StoreBreaker {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = fn
	b.lastStateChange = fn()
	return b
}

// WithRand overrides the random float function used for jitter calculations.
func (b *StoreBreaker) WithRand(fn func() float64) *StoreBreaker {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.randFloat = fn
	return b
}

// State returns the current breaker state, transitioning to half-open if cooldown has elapsed.
func (b *StoreBreaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checkTransitionLocked()
	return b.state
}

// Degraded reports whether the circuit breaker is currently open or half-open.
func (b *StoreBreaker) Degraded() bool {
	return b.State() != StateClosed
}

// Allow reports whether a store operation is permitted.
// If the breaker is open, it returns false to fail fast.
// If the cooldown has expired, it transitions to half-open and allows a single probe.
func (b *StoreBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.checkTransitionLocked()

	switch b.state {
	case StateClosed:
		return true
	case StateHalfOpen:
		if !b.probeInFlight {
			b.probeInFlight = true
			return true
		}
		return false
	case StateOpen:
		return false
	default:
		return true
	}
}

// RemainingCooldown returns the remaining time before a probe is attempted if open.
func (b *StoreBreaker) RemainingCooldown() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != StateOpen {
		return 0
	}
	elapsed := b.now().Sub(b.lastStateChange)
	if elapsed >= b.currentCooldown {
		return 0
	}
	return b.currentCooldown - elapsed
}

// RecordSuccess records a successful database operation, resetting the breaker to closed state.
func (b *StoreBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.state = StateClosed
	b.consecutiveFails = 0
	b.currentCooldown = b.baseCooldown
	b.probeInFlight = false
	b.lastStateChange = b.now()
}

// RecordFailure records a database error. If the error indicates database unavailability,
// it increments consecutive failures and trips the breaker if threshold is reached.
func (b *StoreBreaker) RecordFailure(err error) {
	if err == nil || (!IsUnavailable(err) && err != ErrUnavailable) {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.probeInFlight = false
	b.consecutiveFails++

	switch b.state {
	case StateClosed:
		if b.consecutiveFails >= b.consecutiveLimit {
			b.tripLocked()
		}
	case StateHalfOpen:
		// A probe failure immediately re-opens the breaker with backed-off jittered delay.
		next := b.currentCooldown * 2
		if next > b.maxCooldown {
			next = b.maxCooldown
		}
		b.currentCooldown = next
		b.tripLocked()
	case StateOpen:
		// Remain open, do not overwrite state change time so ongoing cooldown is preserved.
	}
}

func (b *StoreBreaker) tripLocked() {
	b.state = StateOpen
	b.lastStateChange = b.now()
	b.currentCooldown = jitter(b.currentCooldown, b.randFloat)
}

func (b *StoreBreaker) checkTransitionLocked() {
	if b.state == StateOpen && b.now().Sub(b.lastStateChange) >= b.currentCooldown {
		b.state = StateHalfOpen
		b.lastStateChange = b.now()
		b.probeInFlight = false
	}
}
