package notify

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ChannelRateLimiter manages token-bucket rate limiters per channel ID.
// Design decision: per-channel (keyed by channel ID) rather than per-channel-type,
// so two webhooks pointing to different endpoints have independent rate buckets,
// while sharing the researched channel-type default limits.
type ChannelRateLimiter struct {
	defaults map[string]float64
	mu       sync.Mutex
	limiters map[int64]*rate.Limiter
	now      func() time.Time
}

// NewChannelRateLimiter creates a new rate limiter manager with specified default limits per channel type.
func NewChannelRateLimiter(defaults map[string]float64) *ChannelRateLimiter {
	return &ChannelRateLimiter{
		defaults: defaults,
		limiters: make(map[int64]*rate.Limiter),
		now:      time.Now,
	}
}

// GetLimiter returns or initializes a token bucket limiter for the given channel ID and type.
func (l *ChannelRateLimiter) GetLimiter(channelID int64, channelType string, customRPS float64) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	if lim, ok := l.limiters[channelID]; ok {
		return lim
	}

	rps := customRPS
	if rps <= 0 {
		if d, ok := l.defaults[strings.ToLower(channelType)]; ok {
			rps = d
		} else {
			rps = 5.0 // fallback default rps
		}
	}

	burst := int(math.Ceil(rps))
	if burst < 1 {
		burst = 1
	}

	lim := rate.NewLimiter(rate.Limit(rps), burst)
	l.limiters[channelID] = lim
	return lim
}

// ReserveOrBlock checks the token bucket for the channel and reserves a token. If the bucket is empty,
// it respects any Retry-After duration or waits/defers according to the limiter state.
func (l *ChannelRateLimiter) Wait(ctx context.Context, channelID int64, channelType string, customRPS float64) error {
	lim := l.GetLimiter(channelID, channelType, customRPS)
	return lim.Wait(ctx)
}

// ParseRetryAfter parses a Retry-After header value (either seconds or HTTP-date) and returns a duration.
func ParseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second,
			true
	}
	if t, err := httpParseTime(value); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0, true
		}
		return d, true
	}
	return 0, false
}

func httpParseTime(value string) (time.Time, error) {
	// Basic wrapper or layout attempts for HTTP dates if needed
	return time.Parse(time.RFC1123, value)
}
