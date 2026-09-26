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
//
// Every ingest metric carries the Stellar network it belongs to, because one
// instance can now poll several at once and "the poller is lagging" is not a
// question that has an instance-wide answer. The copy returned by WithNetwork
// shares one registry with its parent, so /metrics exports a single merged
// view: the aggregate sum over networks still works, and so does a per-network
// slice.
type Metrics struct {
	registry *prometheus.Registry
	// network is this instance's label value. New leaves it empty, which is
	// how a single-network deployment that never calls WithNetwork reports —
	// the same series name it always had, just with the label unbound.
	network string

	pollsTotal    *prometheus.CounterVec
	pollDuration  *prometheus.HistogramVec
	pollLagLedger *prometheus.GaugeVec

	// Priority tier gauges: how many contracts each tier carries, and how
	// far behind the tip the freshest event seen for that tier is. The lag
	// gauge is labelled by the closed tier set, so cardinality is bounded.
	pollPriorityContracts *prometheus.GaugeVec
	pollPriorityLag       *prometheus.GaugeVec

	// Reorg detection: how many reorgs have been seen, and the ledger of the
	// most recent one. Both are expected to sit at zero. Per network because a
	// reorganisation is a property of one chain, and an operator reading
	// "reorgs_total went up" needs to know which chain moved.
	reorgsTotal     *prometheus.CounterVec
	lastReorgLedger *prometheus.GaugeVec

	eventsScanned  *prometheus.CounterVec
	eventsMatched  *prometheus.CounterVec
	alertsFired    *prometheus.CounterVec
	lastPollAgoSec *prometheus.GaugeVec

	// pollPanics counts ingest cycles that ended in a recovered panic. With
	// one process polling several networks, a panic that is recovered is
	// invisible except that one chain's cursor stops advancing, so it needs
	// its own series to be alertable.
	pollPanics *prometheus.CounterVec

	// Not ingest-scoped: delivery and HTTP labels come from the channel type
	// and the route, and adding a network here would multiply series nobody
	// queries by.
	deliveries   *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

// networkLabel is the Prometheus label every ingest metric carries.
const networkLabel = "network"

// New returns a Metrics with its own registry, so multiple instances (e.g.
// in tests) never collide on the default registry.
func New() *Metrics {
	networks := []string{networkLabel}
	m := &Metrics{
		registry: prometheus.NewRegistry(),

		pollsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_polls_total",
			Help: "RPC polls, by network and outcome (ok|error).",
		}, []string{networkLabel, "outcome"}),

		pollDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sorobeacon_poll_duration_seconds",
			Help:    "Wall-clock duration of a single poll cycle, by network.",
			Buckets: prometheus.DefBuckets,
		}, networks),

		pollLagLedger: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_poll_lag_ledgers",
			Help: "How far a network's poller resume point trails that chain's tip, in ledgers.",
		}, networks),

		pollPriorityContracts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_poll_priority_contracts",
			Help: "Watched contracts per poll-priority tier (low|normal|high), by network.",
		}, []string{networkLabel, "priority"}),

		pollPriorityLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_poll_lag_ledgers_by_priority",
			Help: "Ledger lag of the freshest event seen for each poll-priority tier this cycle, by network.",
		}, []string{networkLabel, "priority"}),

		reorgsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_reorgs_total",
			Help: "Chain reorganisations detected within the tracking window, by network. Expected to stay at zero.",
		}, networks),

		lastReorgLedger: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_last_reorg_ledger",
			Help: "Ledger at which a network most recently diverged from the ingested chain.",
		}, networks),

		eventsScanned: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_events_scanned_total",
			Help: "Contract events read from the RPC and evaluated against rules, by network.",
		}, networks),

		eventsMatched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_events_matched_total",
			Help: "Events that matched at least one monitor rule, by network.",
		}, networks),

		alertsFired: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_alerts_fired_total",
			Help: "Alerts created by rule matches, by network.",
		}, networks),

		lastPollAgoSec: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sorobeacon_seconds_since_last_poll",
			Help: "Seconds since a network's poller last completed a cycle. Grows without bound when polling has stopped.",
		}, networks),

		pollPanics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_poll_panics_total",
			Help: "Ingest cycles ended by a recovered panic, by network. A non-zero value means that chain's poll loop restarted.",
		}, networks),

		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sorobeacon_alert_deliveries_total",
			Help: "Alert deliveries, by channel type and outcome (ok|error).",
		}, []string{"channel", "outcome"}),

		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sorobeacon_http_request_duration_seconds",
			Help:    "HTTP request duration by route pattern, method and status.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method", "status"}),
	}
	m.registry.MustRegister(m.pollsTotal, m.pollDuration, m.pollLagLedger,
		m.eventsScanned, m.eventsMatched, m.alertsFired, m.deliveries,
		m.httpDuration, m.lastPollAgoSec, m.pollPriorityContracts, m.pollPriorityLag,
		m.reorgsTotal, m.lastReorgLedger, m.pollPanics)
	return m
}

// WithNetwork returns a view of the same metric set whose ingest metrics carry
// `network`. Each poller gets one, so two networks polled by one process
// report two labelled series instead of overwriting each other's gauges.
//
// It is a shallow copy on purpose: the registry and every vector are shared,
// so /metrics stays a single scrape target.
func (m *Metrics) WithNetwork(network string) *Metrics {
	if m == nil {
		return nil
	}
	dup := *m
	dup.network = network
	return &dup
}

// lv prefixes a metric's label values with this view's network.
func (m *Metrics) lv(values ...string) []string {
	return append([]string{m.network}, values...)
}

// Handler serves the metrics registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
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
	m.pollsTotal.WithLabelValues(m.lv(outcome)...).Inc()
	m.pollDuration.WithLabelValues(m.lv()...).Observe(took.Seconds())
	m.lastPollAgoSec.WithLabelValues(m.lv()...).Set(0)
}

// SetPollLag records the gap between the poller's resume point and the
// chain tip.
func (m *Metrics) SetPollLag(ledgersBehind int64) {
	if m == nil {
		return
	}
	m.pollLagLedger.WithLabelValues(m.lv()...).Set(float64(ledgersBehind))
}

// SetPriorityContracts records how many contracts the cycle scheduled in each
// priority tier. It is a gauge, not a counter: contracts are added and removed
// constantly, so the useful question is "how big is this tier now".
func (m *Metrics) SetPriorityContracts(priority string, contracts int) {
	if m == nil {
		return
	}
	m.pollPriorityContracts.WithLabelValues(m.lv(priority)...).Set(float64(contracts))
}

// RecordReorg counts one detected chain reorganisation and records where it
// diverged.
func (m *Metrics) RecordReorg(ledger uint32) {
	if m == nil {
		return
	}
	m.reorgsTotal.WithLabelValues(m.lv()...).Inc()
	m.lastReorgLedger.WithLabelValues(m.lv()...).Set(float64(ledger))
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
	m.pollPriorityLag.WithLabelValues(m.lv(priority)...).Set(float64(ledgersBehind))
}

// TickPollAge advances the seconds-since-last-poll gauge; called on a timer
// so the gauge climbs visibly when polling has stalled.
func (m *Metrics) TickPollAge(secondsSince float64) {
	if m == nil {
		return
	}
	m.lastPollAgoSec.WithLabelValues(m.lv()...).Set(secondsSince)
}

// RecordEvents counts events read and how many matched a rule in the cycle
// that just ran.
func (m *Metrics) RecordEvents(scanned, matched int) {
	if m == nil {
		return
	}
	m.eventsScanned.WithLabelValues(m.lv()...).Add(float64(scanned))
	m.eventsMatched.WithLabelValues(m.lv()...).Add(float64(matched))
}

// RecordAlert counts one alert fired.
func (m *Metrics) RecordAlert() {
	if m == nil {
		return
	}
	m.alertsFired.WithLabelValues(m.lv()...).Inc()
}

// RecordPanic counts one ingest cycle that ended in a recovered panic, so
// the supervisor's restart is visible in monitoring rather than only in the
// log.
func (m *Metrics) RecordPanic() {
	if m == nil {
		return
	}
	m.pollPanics.WithLabelValues(m.lv()...).Inc()
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
