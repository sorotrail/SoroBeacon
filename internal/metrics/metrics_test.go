package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metricNames is every family the endpoint is expected to expose. Keeping it
// here means a renamed or dropped metric fails the test rather than silently
// disappearing from a dashboard.
var metricNames = []string{
	"sorobeacon_polls_total",
	"sorobeacon_poll_duration_seconds",
	"sorobeacon_poll_lag_ledgers",
	"sorobeacon_seconds_since_last_poll",
	"sorobeacon_events_scanned_total",
	"sorobeacon_events_matched_total",
	"sorobeacon_rule_evaluations_total",
	"sorobeacon_alerts_fired_total",
	"sorobeacon_alert_deliveries_total",
	"sorobeacon_http_request_duration_seconds",
}

// scrape renders the metrics endpoint into text.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// TestNewRegistersCollectors is the guard the issue calls for: duplicate
// registration on one registry is the classic startup panic, so New must be
// safe to call repeatedly (it builds a fresh registry each time).
func TestNewRegistersCollectors(t *testing.T) {
	for i := 0; i < 3; i++ {
		m := New()
		require.NotNil(t, m.Handler())
	}
}

// TestHandlerExposesPipelineMetrics exercises every hook and asserts the
// endpoint renders each metric in Prometheus text format.
func TestHandlerExposesPipelineMetrics(t *testing.T) {
	m := New()
	m.RecordPoll(true, 250*time.Millisecond)
	m.RecordPoll(false, time.Second)
	m.SetPollLag(12)
	m.TickPollAge(3)
	m.RecordEvents(10, 4)
	m.RecordRuleEvaluations(25)
	m.RecordAlert()
	m.RecordDelivery("slack", true)
	m.RecordDelivery("slack", false)
	// The HTTP histogram only appears once a request has been observed.
	m.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	body := scrape(t, m)
	for _, name := range metricNames {
		assert.Contains(t, body, name)
	}
	assert.Contains(t, body, `outcome="ok"`)
	assert.Contains(t, body, `outcome="error"`)
	assert.Contains(t, body, `channel="slack"`)
	assert.Contains(t, body, "sorobeacon_alerts_fired_total 1")
}

// TestDeliveryLabelsAreBounded pins the cardinality rule: deliveries are
// labelled by channel type and outcome only — never by channel id, contract
// id or event id, which would let a busy chain grow the series count without
// bound.
func TestDeliveryLabelsAreBounded(t *testing.T) {
	m := New()
	m.RecordDelivery("slack", true)
	m.RecordDelivery("slack", false)
	m.RecordDelivery("discord", true)

	families, err := m.registry.Gather()
	require.NoError(t, err)

	var found *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "sorobeacon_alert_deliveries_total" {
			found = f
		}
	}
	require.NotNil(t, found, "deliveries metric must be registered")
	require.NotEmpty(t, found.GetMetric(), "recorded deliveries must appear")

	var labels []string
	for _, lp := range found.GetMetric()[0].GetLabel() {
		labels = append(labels, lp.GetName())
	}
	assert.ElementsMatch(t, []string{"channel", "outcome"}, labels)
	assert.Len(t, found.GetMetric(), 3, "one series per channel/outcome pair")
}

// TestMiddlewareUsesRoutePattern guards HTTP label cardinality: the route
// label must be the matched pattern, not the raw path (which embeds IDs).
func TestMiddlewareUsesRoutePattern(t *testing.T) {
	m := New()
	RoutePattern = func(*http.Request) string { return "/api/v1/monitors/{id}" }
	t.Cleanup(func() { RoutePattern = nil })

	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/monitors/42", nil))

	body := scrape(t, m)
	assert.Contains(t, body, `route="/api/v1/monitors/{id}"`)
	assert.Contains(t, body, `status="418"`)
	assert.NotContains(t, body, `route="/api/v1/monitors/42"`)
}

// TestNilMetricsIsSafe pins the "instrumentation is never load-bearing"
// contract: call sites hold the pointer without nil checks, so a nil
// *Metrics must record nothing instead of panicking.
func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	assert.NotPanics(t, func() {
		m.RecordPoll(true, time.Second)
		m.SetPollLag(1)
		m.TickPollAge(1)
		m.RecordEvents(1, 1)
		m.RecordRuleEvaluations(1)
		m.RecordAlert()
		m.RecordDelivery("slack", true)
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
		m.Middleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
}
