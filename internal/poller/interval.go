package poller

import "time"

// MinPollInterval is the hard floor on every poll delay, adaptive or not.
// The RPC is not worth hitting more often than this.
const MinPollInterval = time.Second

// AdjustInterval returns the next poll delay after a cycle.
//
// Policy (documented so operators can reason about POLL_INTERVAL_MIN/MAX):
//   - min or max <= 0 disables adaptation and returns current unchanged
//     (existing deployments keep a fixed POLL_INTERVAL).
//   - backlog (events arrived, or the checkpoint trails the chain tip)
//     halves the interval toward min.
//   - idle cycles double the interval toward max.
//   - the result is always inside [min, max] and never below MinPollInterval.
//
// Tests drive this directly with synthetic outcomes — no sleeping.
func AdjustInterval(current, min, max time.Duration, backlog bool) time.Duration {
	if min <= 0 || max <= 0 {
		return current
	}
	if min < MinPollInterval {
		min = MinPollInterval
	}
	if max < min {
		max = min
	}
	var next time.Duration
	switch {
	case backlog:
		next = current / 2
		if next < min {
			next = min
		}
	case current > max/2:
		next = max
	default:
		next = current * 2
		if next < current { // overflow
			next = max
		}
	}
	if next < min {
		next = min
	}
	if next > max {
		next = max
	}
	if next < MinPollInterval {
		next = MinPollInterval
	}
	return next
}

// clampInterval keeps v inside [min, max] with the 1s floor. Used for the
// starting delay so POLL_INTERVAL cannot sit outside the configured band.
func clampInterval(v, min, max time.Duration) time.Duration {
	if min <= 0 || max <= 0 {
		if v < MinPollInterval {
			return MinPollInterval
		}
		return v
	}
	if min < MinPollInterval {
		min = MinPollInterval
	}
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	return v
}
