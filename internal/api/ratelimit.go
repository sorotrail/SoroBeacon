package api

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitConfig is the per-process limiter. RPS <= 0 disables it, which
// is the default so existing deployments are unaffected until an operator
// opts in with RATE_LIMIT_RPS.
type RateLimitConfig struct {
	RPS            float64
	Burst          int
	TrustForwarded bool
	// IdleTTL is how long a per-client bucket may sit unused before it is
	// evicted. Zero means the default (3 minutes).
	IdleTTL time.Duration
}

const defaultRateLimitIdleTTL = 3 * time.Minute

type visitor struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// RateLimiter is a token-bucket limiter keyed by client address. The map
// is bounded by evicting idle entries so a flood of distinct addresses
// cannot grow it without bound.
type RateLimiter struct {
	cfg      RateLimitConfig
	mu       sync.Mutex
	visitors map[string]*visitor
	now      func() time.Time
}

func newRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = defaultRateLimitIdleTTL
	}
	if cfg.Burst <= 0 {
		cfg.Burst = int(math.Ceil(cfg.RPS))
		if cfg.Burst < 1 {
			cfg.Burst = 1
		}
	}
	return &RateLimiter{
		cfg:      cfg,
		visitors: make(map[string]*visitor),
		now:      time.Now,
	}
}

func (l *RateLimiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.visitors)
}

func (l *RateLimiter) get(key string) *rate.Limiter {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
	v, ok := l.visitors[key]
	if !ok {
		v = &visitor{lim: rate.NewLimiter(rate.Limit(l.cfg.RPS), l.cfg.Burst)}
		l.visitors[key] = v
	}
	v.lastSeen = now
	return v.lim
}

func (l *RateLimiter) sweepLocked(now time.Time) {
	for k, v := range l.visitors {
		if now.Sub(v.lastSeen) >= l.cfg.IdleTTL {
			delete(l.visitors, k)
		}
	}
}

func isProbePath(path string) bool {
	switch {
	case strings.HasSuffix(path, "/health") ||
		strings.HasSuffix(path, "/livez") ||
		strings.HasSuffix(path, "/readyz"):
		return true
	default:
		return false
	}
}

func clientKey(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// First hop is the original client; later hops are proxies.
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func retryAfterSeconds(rps float64) int {
	if rps <= 0 {
		return 1
	}
	n := int(math.Ceil(1 / rps))
	if n < 1 {
		return 1
	}
	return n
}

func setRateLimitHeaders(w http.ResponseWriter, limit, remaining, reset int) {
	w.Header().Set("RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("RateLimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("RateLimit-Reset", strconv.Itoa(reset))
}

// RateLimitMiddleware applies a per-client token bucket to API routes.
// Health, liveness and readiness stay exempt so an orchestrator cannot be
// rate-limited into restarting a healthy instance. Disabled (RPS <= 0) is
// a no-op.
func RateLimitMiddleware(cfg RateLimitConfig) func(http.Handler) http.Handler {
	if cfg.RPS <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	lim := newRateLimiter(cfg)
	return lim.Middleware
}

// Middleware is the http.Handler wrapper for a constructed RateLimiter.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		bucket := l.get(clientKey(r, l.cfg.TrustForwarded))
		if !bucket.Allow() {
			retry := retryAfterSeconds(l.cfg.RPS)
			setRateLimitHeaders(w, l.cfg.Burst, 0, retry)
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeErr(w, r, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		remaining := int(bucket.Tokens())
		if remaining < 0 {
			remaining = 0
		}
		setRateLimitHeaders(w, l.cfg.Burst, remaining, 0)
		next.ServeHTTP(w, r)
	})
}
