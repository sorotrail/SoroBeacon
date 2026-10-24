package poller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/metrics"
)

// PositionReader is the race-free ingest snapshot the heartbeat watches. It
// is the same shape /readyz and the dashboard consume, so "healthy" has one
// definition across the process rather than a second notion invented here.
type PositionReader interface {
	Position() Position
}

// heartbeatTimeout bounds one ping, and one store reachability check, so a
// slow or black-holed heartbeat endpoint can never delay the poll loop that
// shares this process. Five seconds is generous for a GET and still short
// enough that a hung endpoint costs at most one interval's grace.
const heartbeatTimeout = 5 * time.Second

// DefaultHeartbeatInterval is how often a healthy instance pings when
// HEARTBEAT_INTERVAL is unset. A minute matches what the dead-man's-switch
// services default to, and it is comfortably longer than a poll cycle, which
// the success condition needs: the ping is sent only when a new successful
// poll has appeared since the previous check.
const DefaultHeartbeatInterval = time.Minute

// HeartbeatConfig is the configured shape of the watcher, as Load produced
// it. URL is a secret — the path identifies the monitor to the external
// service — so it never reaches a log line, an error string or a metric.
type HeartbeatConfig struct {
	// URL is the success endpoint. Empty disables the watcher.
	URL string
	// Interval is how often the health of ingest is re-evaluated.
	Interval time.Duration
	// FailureSuffix, when non-empty, enables one failure ping per outage
	// to URL+suffix (Healthchecks.io's "/fail" convention). Empty means
	// success pings only: the external service then notices the silence
	// on its own timeout instead of being told explicitly.
	FailureSuffix string
	// LagThreshold is READYZ_LAG_THRESHOLD reused for the same question:
	// at what ledger lag is ingest no longer "flowing". Zero means the
	// check is disabled, exactly as it is for /readyz.
	LagThreshold uint32
}

// Heartbeat pings an external dead-man's switch while — and only while —
// ingest is demonstrably healthy. The design point is what a ping proves: a
// ping from a timer says only that the process is alive, so this watcher
// sends its ping only after the poller has completed a successful cycle
// since the last check. A wedged poller, a stalled RPC or an unreachable
// store stops the pings, and the external service alerts on the silence.
//
// It runs inside the lease-gated job (see cmd/sorobeacon), so with several
// replicas only the leader ever pings, and a leadership hand-over starts the
// new leader's watcher with an immediate check — the gap between an old
// leader's last ping and a new leader's first cannot exceed one interval.
type Heartbeat struct {
	url      string
	interval time.Duration
	// suffix is the failure-path suffix; empty disables failure pings.
	suffix      string
	threshold   uint32
	position    PositionReader
	pingStore   func(context.Context) error
	client      *http.Client
	log         *slog.Logger
	metrics     *metrics.Metrics
	failureSent bool
	// wasHealthy records that at least one success ping has been sent. A
	// failure ping is only meaningful once the external service has been
	// told life is good — before that, silence already means "no data".
	wasHealthy bool
	// now is the clock, overridable so a test can step time instead of
	// sleeping through a real interval; production leaves it at New's
	// default.
	now func() time.Time
}

// NewHeartbeat builds the watcher. pingStore is the store reachability
// check (store.Ping); a nil metrics is fine and simply records nothing.
func NewHeartbeat(cfg HeartbeatConfig, pos PositionReader, pingStore func(context.Context) error, m *metrics.Metrics, log *slog.Logger) *Heartbeat {
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	return &Heartbeat{
		url:       cfg.URL,
		interval:  interval,
		suffix:    cfg.FailureSuffix,
		threshold: cfg.LagThreshold,
		position:  pos,
		pingStore: pingStore,
		client:    &http.Client{Timeout: heartbeatTimeout},
		log:       log,
		now:       time.Now,
		metrics:   m,
	}
}

// Run checks the health of ingest immediately and then once per interval
// until ctx is cancelled. The first check is immediate so a leader elected
// mid-interval does not leave the external service waiting: leadership
// changes cannot open a gap longer than one interval, which is what keeps a
// failover from itself looking like an outage.
func (h *Heartbeat) Run(ctx context.Context) {
	if h.url == "" {
		return
	}
	// prev is the LastSuccessfulPoll the previous check saw. It starts one
	// interval in the past so the immediate first check accepts a poll that
	// completed just before this watcher started.
	prev := h.now().Add(-h.interval)
	check := func() time.Time {
		ok, reason := h.check(ctx, prev)
		switch {
		case ok:
			h.ping(ctx, h.url, true)
			h.wasHealthy = true
			h.failureSent = false
		case h.failureSent:
			// Already inside an outage: one failure ping per outage, so a
			// long outage does not hammer the external service.
		default:
			// Before the first success there is nothing to fail: the
			// external service learns from the missing pings. Once pings
			// have flowed, silence would be ambiguous, so an explicit
			// failure ping tells the service this is a known-bad state —
			// but only when failure pings are configured at all; with no
			// suffix the silence is the signal.
			if h.wasHealthy && h.suffix != "" {
				h.ping(ctx, h.url+h.suffix, false)
				h.failureSent = true
			}
			h.log.Warn("heartbeat: ingest is not healthy; success pings are paused",
				"reason", reason)
		}
		return h.position.Position().LastSuccessfulPoll
	}
	prev = check()

	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prev = check()
		}
	}
}

// check reports whether ingest is healthy enough to ping, and why not when
// it is not. Healthy means: the poller has completed a successful cycle
// since the previous check (progress, not mere liveness), lag is within the
// readiness threshold when one is configured, and the store answers a ping.
// The store check runs last because it is the only one that touches the
// network: the position snapshot is pure memory.
func (h *Heartbeat) check(ctx context.Context, prev time.Time) (bool, string) {
	if h.position == nil {
		return false, "no poller position source"
	}
	pos := h.position.Position()
	if !pos.Ready() {
		return false, "no successful poll yet"
	}
	if !pos.LastSuccessfulPoll.After(prev) {
		return false, "poller has not completed a successful cycle since the last check"
	}
	if h.threshold > 0 && pos.Lag() > int64(h.threshold) {
		return false, "ledger lag exceeds the readiness threshold"
	}
	if h.pingStore != nil {
		ctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
		defer cancel()
		if err := h.pingStore(ctx); err != nil {
			return false, "store is unreachable"
		}
	}
	return true, ""
}

// ping performs one GET and records the outcome. success marks a healthy
// check and stamps the metric; a failure ping never touches the metric. The
// request itself is bounded by the client timeout, so a slow endpoint costs
// this goroutine at most heartbeatTimeout and never the poll loop.
func (h *Heartbeat) ping(ctx context.Context, target string, success bool) {
	if target == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		// The error text can embed the URL, which is a secret; say what
		// happened without repeating it.
		h.log.Error("heartbeat: configured URL is not a usable request",
			"err_class", classifyPingErr(err))
		return
	}
	res, err := h.client.Do(req)
	if err != nil {
		h.log.Warn("heartbeat: ping failed", "err_class", classifyPingErr(err))
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		_ = res.Body.Close()
	}()
	if res.StatusCode >= 400 {
		h.log.Warn("heartbeat: ping rejected", "status", res.StatusCode)
		return
	}
	if success {
		if h.metrics != nil {
			h.metrics.RecordHeartbeatSuccess()
		}
		h.log.Debug("heartbeat: ping sent")
		return
	}
	h.log.Info("heartbeat: failure ping sent")
}

// now is the clock the watcher reads; New defaults it to time.Now, and a
// test may override it to step time instead of sleeping.

// classifyPingErr turns a transport error into a category whose text can
// never contain the URL: net/url errors embed the target, and the target is
// the credential. Categories keep the log useful without leaking it.
func classifyPingErr(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	switch {
	case err == nil:
		return "request"
	case strings.Contains(err.Error(), "certificate"):
		return "tls_error"
	case strings.Contains(err.Error(), "no such host"):
		return "dns_error"
	case strings.Contains(err.Error(), "connection refused"):
		return "connection_refused"
	default:
		return "transport_error"
	}
}
