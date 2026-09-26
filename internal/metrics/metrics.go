// Package metrics holds SoroBeacon's Prometheus instrumentation.
//
// Every method is safe on a nil *Metrics, so call sites can hold the
// pointer without nil checks and tests can pass nil. A nil Metrics records
// nothing — instrumentation is never load-bearing.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the process-wide metric set. Construct with New; serve with
// Handler.
type Metrics struct {
	registry *prometheus.Registry

	pollsTotal    *prometheus.CounterVec
	pollDuration  prometheus.Histogram
	pollLagLedger prometheus.Gauge

	// Priority tier gauges: how many contracts each tier carries, and how
	// far behind the tip the freshest event seen for that tier is. The lag
	// gauge is labelled by the closed tier set, so cardinality is bounded.
	pollPriorityContracts *prometheus.GaugeVec
	pollPriorityLag       *prometheus.GaugeVec

	// Reorg detection: how many reorgs have been seen, and the ledger of the
	// most recent one. Both are expected to sit at zero.
	reorgsTotal     prometheus.Counter
	lastReorgLedger prometheus.Gauge

	eventsScanned  prometheus.Counter
	eventsMatched  prometheus.Counter
	alertsFired    prometheus.Counter
	deliveries     *prometheus.CounterVec
	throttles      *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	lastPollAgoSec prometheus.Gauge

	storeReads     *prometheus.CounterVec
	storeFallbacks prometheus.Counter
	replicaEnabled prometheus.Gauge
}

// New returns a Metrics with its own registry, so multiple instances (e.g.
// in tests) never collide on the default registry.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),

		pollsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_polls_total",
			Help: "RPC polls, by outcome (ok|error).",
		}, []string{"outcome"}),

		pollDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sorobeacon_poll_duration_seconds",
			Help:    "Wall-clock duration of a single poll cycle.",
			Buckets: prometheus.DefBuckets,
		}),

		pollLagLedger: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sorobeacon_poll_lag_ledgers",
			Help: "How far the poller's resume point trails the chain tip, in ledgers.",
		}),

		pollPriorityContracts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_poll_priority_contracts",
			Help: "Watched contracts per poll-priority tier (low|normal|high).",
		}, []string{"priority"}),

		pollPriorityLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_poll_lag_ledgers_by_priority",
			Help: "Ledger lag of the freshest event seen for each poll-priority tier this cycle.",
		}, []string{"priority"}),

		reorgsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sorobeacon_reorgs_total",
			Help: "Chain reorganisations detected within the tracking window. Expected to stay at zero.",
		}),

		lastReorgLedger: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sorobeacon_last_reorg_ledger",
			Help: "Ledger at which the most recent reorganisation diverged from the ingested chain.",
		}),

		eventsScanned: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sorobeacon_events_scanned_total",
			Help: "Contract events read from the RPC and evaluated against rules.",
		}),

		eventsMatched: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sorobeacon_events_matched_total",
			Help: "Events that matched at least one monitor rule.",
		}),

		alertsFired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sorobeacon_alerts_fired_total",
			Help: "Alerts created by rule matches.",
		}),

		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_alert_deliveries_total",
			Help: "Alert deliveries, by channel type and outcome (ok|error).",
		}, []string{"channel", "outcome"}),

		throttles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_alert_throttles_total",
			Help: "Throttled alert delivery attempts, by channel type.",
		}, []string{"channel"}),

		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sorobeacon_http_request_duration_seconds",
			Help:    "HTTP request duration by route pattern, method and status.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method", "status"}),

		lastPollAgoSec: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sorobeacon_seconds_since_last_poll",
			Help: "Seconds since the poller last completed a cycle. Grows without bound when polling has stopped.",
		}),

		// Where read-only queries actually went. The pool label is the closed
		// set {primary, replica}, so cardinality is bounded; watching the ratio
		// is how an operator confirms replica routing is doing anything, and
		// the fallback counter is how they see it stop.
		storeReads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_store_reads_total",
			Help: "Read-only store queries by the pool that served them.",
		}, []string{"pool"}),

		storeFallbacks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sorobeacon_store_replica_fallbacks_total",
			Help: "Read-only queries that were served by the primary because the replica was unavailable.",
		}),

		replicaEnabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sorobeacon_store_replica_enabled",
			Help: "1 when a read replica is configured and routing, 0 when every read goes to the primary.",
		}),
	}
	m.registry.MustRegister(m.pollsTotal, m.pollDuration, m.pollLagLedger,
		m.eventsScanned, m.eventsMatched, m.alertsFired, m.deliveries, m.throttles,
		m.httpDuration, m.lastPollAgoSec, m.pollPriorityContracts, m.pollPriorityLag,
		m.reorgsTotal, m.lastReorgLedger,
		m.storeReads, m.storeFallbacks, m.replicaEnabled)
	return m
}

// Handler serves the metrics registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// RegisterStreamDropped exposes the live-alerts broadcaster's dropped-event
// counter on /metrics. The callback is read lazily on each scrape, so the
// broadcaster keeps its own cheap atomic counter and this package does not
// need to import it. A nil callback (or nil receiver) registers nothing.
func (m *Metrics) RegisterStreamDropped(dropped func() uint64) {
	if m == nil || dropped == nil {
		return
	}
	m.registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "sorobeacon_alerts_stream_dropped_total",
		Help: "Alert events dropped from the live SSE stream because a subscriber could not keep up.",
	}, func() float64 {
		return float64(dropped())
	}))
}

// RecordPoll observes one completed poll cycle.
func (m *Metrics) RecordPoll(ok bool, took time.Duration) {
	if m == nil {
		return
	}
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	m.pollsTotal.WithLabelValues(outcome).Inc()
	m.pollDuration.Observe(took.Seconds())
	m.lastPollAgoSec.Set(0)
}

// SetPollLag records the gap between the poller's resume point and the
// chain tip.
func (m *Metrics) SetPollLag(ledgersBehind int64) {
	if m == nil {
		return
	}
	m.pollLagLedger.Set(float64(ledgersBehind))
}

// SetPriorityContracts records how many contracts the cycle scheduled in each
// priority tier. It is a gauge, not a counter: contracts are added and removed
// constantly, so the useful question is "how big is this tier now".
func (m *Metrics) SetPriorityContracts(priority string, contracts int) {
	if m == nil {
		return
	}
	m.pollPriorityContracts.WithLabelValues(priority).Set(float64(contracts))
}

// RecordReorg counts one detected chain reorganisation and records where it
// diverged.
func (m *Metrics) RecordReorg(ledger uint32) {
	if m == nil {
		return
	}
	m.reorgsTotal.Inc()
	m.lastReorgLedger.Set(float64(ledger))
}

// SetPollLagByPriority records the ledger lag of the freshest event seen for
// one priority tier in the cycle that just finished. A tier whose contracts
// are scheduled later in a slow cycle shows a larger lag here even though the
// cycle-wide lag is the same, which is what makes the scheduling effect
// measurable.
func (m *Metrics) SetPollLagByPriority(priority string, ledgersBehind int64) {
	if m == nil {
		return
	}
	if ledgersBehind < 0 {
		ledgersBehind = 0
	}
	m.pollPriorityLag.WithLabelValues(priority).Set(float64(ledgersBehind))
}

// TickPollAge advances the seconds-since-last-poll gauge; called on a timer
// so the gauge climbs visibly when polling has stalled.
func (m *Metrics) TickPollAge(secondsSince float64) {
	if m == nil {
		return
	}
	m.lastPollAgoSec.Set(secondsSince)
}

// RecordEvents counts events read and how many matched a rule in the cycle
// that just ran.
func (m *Metrics) RecordEvents(scanned, matched int) {
	if m == nil {
		return
	}
	m.eventsScanned.Add(float64(scanned))
	m.eventsMatched.Add(float64(matched))
}

// RecordAlert counts one alert fired.
func (m *Metrics) RecordAlert() {
	if m == nil {
		return
	}
	m.alertsFired.Inc()
}

// RecordDelivery counts one delivery attempt per channel type and outcome.
// channelType is the notifier kind (discord, slack, telegram, email,
// webhook) — a static set, never request-derived, so cardinality stays
// bounded.
func (m *Metrics) RecordDelivery(channelType string, ok bool) {
	if m == nil {
		return
	}
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	m.deliveries.WithLabelValues(channelType, outcome).Inc()
}

// RecordThrottle counts one throttled delivery per channel type.
func (m *Metrics) RecordThrottle(channelType string) {
	if m == nil {
		return
	}
	m.throttles.WithLabelValues(channelType).Inc()
}

// statusRecorder captures the status code a handler wrote, for the HTTP
// duration metric.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Middleware wraps a handler with per-route HTTP duration instrumentation.
// route is the chi URL pattern, not the raw path, so cardinality stays
// bounded.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := r.URL.Path
		if p := RoutePattern(r); p != "" {
			route = p
		}
		m.httpDuration.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).
			Observe(time.Since(start).Seconds())
	})
}

// RoutePattern extracts the matched route pattern from the request
// context. Implemented via the RoutePatternProvider interface so this
// package does not import chi; the wire-up lives in cmd/sorobeacon.
var RoutePattern func(*http.Request) string
