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

	eventsScanned  prometheus.Counter
	eventsMatched  prometheus.Counter
	alertsFired    prometheus.Counter
	deliveries     *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	lastPollAgoSec prometheus.Gauge
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

		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sorobeacon_http_request_duration_seconds",
			Help:    "HTTP request duration by route pattern, method and status.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method", "status"}),

		lastPollAgoSec: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sorobeacon_seconds_since_last_poll",
			Help: "Seconds since the poller last completed a cycle. Grows without bound when polling has stopped.",
		}),
	}
	m.registry.MustRegister(m.pollsTotal, m.pollDuration, m.pollLagLedger,
		m.eventsScanned, m.eventsMatched, m.alertsFired, m.deliveries,
		m.httpDuration, m.lastPollAgoSec)
	return m
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
