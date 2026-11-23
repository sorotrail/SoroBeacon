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

// Built-in per-provider delivery rates. They are shared by the dispatcher and
// the operator documentation so an unset NOTIFY_RATE_LIMIT_* variable keeps the
// researched value in one place.
const (
	// DefaultSlackRPS follows Slack's webhook guidance of roughly one message
	// per second.
	DefaultSlackRPS = 1.0
	// DefaultTelegramRPS follows the Telegram Bot API limit of 30 messages per
	// second.
	DefaultTelegramRPS = 30.0
	// DefaultPagerDutyRPS follows the PagerDuty Events API v2 limit of roughly
	// two requests per second.
	DefaultPagerDutyRPS = 2.0
	// DefaultFallbackRPS applies to every other channel type.
	DefaultFallbackRPS = 5.0
)

// RateLimits is the set of NOTIFY_RATE_LIMIT_* overrides. A zero or negative
// field means "use the built-in default"; it does not disable delivery. Config
// loading rejects negative values before they reach this type, so zero is the
// value an unset variable produces.
type RateLimits struct {
	SlackRPS     float64
	TelegramRPS  float64
	PagerDutyRPS float64
	DefaultRPS   float64
}

// NewRateLimiter builds the per-channel limiter from operator overrides,
// filling unset values from the built-in defaults.
func NewRateLimiter(r RateLimits) *ChannelRateLimiter {
	slack := r.SlackRPS
	if slack <= 0 {
		slack = DefaultSlackRPS
	}
	telegram := r.TelegramRPS
	if telegram <= 0 {
		telegram = DefaultTelegramRPS
	}
	pagerDuty := r.PagerDutyRPS
	if pagerDuty <= 0 {
		pagerDuty = DefaultPagerDutyRPS
	}
	fallback := r.DefaultRPS
	if fallback <= 0 {
		fallback = DefaultFallbackRPS
	}
	return newChannelRateLimiter(map[string]float64{
		"slack":     slack,
		"telegram":  telegram,
		"pagerduty": pagerDuty,
	}, fallback)
}

// ChannelRateLimiter manages token-bucket rate limiters per channel ID.
// Design decision: per-channel (keyed by channel ID) rather than per-channel-type,
// so two webhooks pointing to different endpoints have independent rate buckets,
// while sharing the researched channel-type default limits.
type ChannelRateLimiter struct {
	defaults map[string]float64
	fallback float64
	mu       sync.Mutex
	limiters map[int64]*rate.Limiter
	now      func() time.Time
}

// NewChannelRateLimiter creates a new rate limiter manager with specified default limits per channel type.
// Unknown types use DefaultFallbackRPS.
func NewChannelRateLimiter(defaults map[string]float64) *ChannelRateLimiter {
	return newChannelRateLimiter(defaults, DefaultFallbackRPS)
}

func newChannelRateLimiter(defaults map[string]float64, fallback float64) *ChannelRateLimiter {
	if fallback <= 0 {
		fallback = DefaultFallbackRPS
	}
	return &ChannelRateLimiter{
		defaults: defaults,
		fallback: fallback,
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
		rps = l.fallback
		if d, ok := l.defaults[strings.ToLower(channelType)]; ok && d > 0 {
			rps = d
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

// Remove deletes the rate limiter bucket for a channel that has been deleted.
func (l *ChannelRateLimiter) Remove(channelID int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.limiters, channelID)
	l.mu.Unlock()
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
