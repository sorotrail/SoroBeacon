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

// The store cannot import these names (they are its own unexported constants),
// so the test and the store agree on the label values by string. If either
// side renames one, these tests fail rather than the dashboard silently going
// blank.
const (
	poolNamePrimary = "primary"
	poolNameReplica = "replica"

	storeReadsName        = "sorobeacon_store_reads_total"
	storeFallbacksName    = "sorobeacon_store_replica_fallbacks_total"
	replicaEnabledName    = "sorobeacon_store_replica_enabled"
	pollsName             = "sorobeacon_polls_total"
	pollDurationName      = "sorobeacon_poll_duration_seconds"
	pollLagName           = "sorobeacon_poll_lag_ledgers"
	pollAgeName           = "sorobeacon_seconds_since_last_poll"
	eventsScannedName     = "sorobeacon_events_scanned_total"
	eventsMatchedName     = "sorobeacon_events_matched_total"
	ruleEvaluationsName   = "sorobeacon_rule_evaluations_total"
	alertsFiredName       = "sorobeacon_alerts_fired_total"
	deliveriesName        = "sorobeacon_alert_deliveries_total"
	priorityContractsName = "sorobeacon_poll_priority_contracts"
	priorityLagName       = "sorobeacon_poll_lag_ledgers_by_priority"
	reorgsName            = "sorobeacon_reorgs_total"
	lastReorgLedgerName   = "sorobeacon_last_reorg_ledger"
	throttlesName         = "sorobeacon_alert_throttles_total"
	breakerStateName      = "sorobeacon_channel_breaker_state"
	httpDurationName      = "sorobeacon_http_request_duration_seconds"
	streamDroppedName     = "sorobeacon_alerts_stream_dropped_total"
)

func TestRecordPollTracksOutcomesAndDuration(t *testing.T) {
	m := New()
	m.TickPollAge(10)

	m.RecordPoll(true, 1250*time.Millisecond)
	m.RecordPoll(false, 2*time.Second)

	assert.Equal(t, float64(1), m.sampleValue(t, pollsName, map[string]string{"outcome": "ok"}))
	assert.Equal(t, float64(1), m.sampleValue(t, pollsName, map[string]string{"outcome": "error"}))
	histogram := m.sampleHistogram(t, pollDurationName, nil)
	assert.Equal(t, uint64(2), histogram.GetSampleCount())
	assert.InDelta(t, 3.25, histogram.GetSampleSum(), 0.000001)
	assert.Equal(t, float64(0), m.sampleValue(t, pollAgeName, nil))
}

func TestSetPollLagRecordsGauge(t *testing.T) {
	m := New()

	m.SetPollLag(17)

	assert.Equal(t, float64(17), m.sampleValue(t, pollLagName, nil))
}

func TestTickPollAgeRecordsGauge(t *testing.T) {
	m := New()

	m.TickPollAge(4.5)

	assert.Equal(t, float64(4.5), m.sampleValue(t, pollAgeName, nil))
}

func TestRecordEventsCountsScannedAndMatched(t *testing.T) {
	m := New()

	m.RecordEvents(9, 3)

	assert.Equal(t, float64(9), m.sampleValue(t, eventsScannedName, nil))
	assert.Equal(t, float64(3), m.sampleValue(t, eventsMatchedName, nil))
}

// Rule evaluations are counted separately from events so a monitor with many
// rules is distinguishable from a busy contract: the same three events give a
// different evaluation count depending on how many rules they are checked
// against.
func TestRecordRuleEvaluationsCountsEvaluations(t *testing.T) {
	m := New()

	m.RecordEvents(3, 1)
	m.RecordRuleEvaluations(9)

	assert.Equal(t, float64(3), m.sampleValue(t, eventsScannedName, nil))
	assert.Equal(t, float64(9), m.sampleValue(t, ruleEvaluationsName, nil))
}

func TestRecordRuleEvaluationsIsNilSafe(t *testing.T) {
	var m *Metrics

	assert.NotPanics(t, func() { m.RecordRuleEvaluations(5) })
}

func TestRecordAlertCountsAlerts(t *testing.T) {
	m := New()

	m.RecordAlert()
	m.RecordAlert()

	assert.Equal(t, float64(2), m.sampleValue(t, alertsFiredName, nil))
}

func TestRecordDeliveryLabelsByChannelType(t *testing.T) {
	m := New()

	m.RecordDelivery("slack", true)
	m.RecordDelivery("slack", true)
	m.RecordDelivery("email", false)

	assert.Equal(t, float64(2), m.sampleValue(t, deliveriesName, map[string]string{"channel": "slack", "outcome": "ok"}))
	assert.Equal(t, float64(1), m.sampleValue(t, deliveriesName, map[string]string{"channel": "email", "outcome": "error"}))
}

func TestHandlerServesMetrics(t *testing.T) {
	m := New()
	m.RecordAlert()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	m.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), alertsFiredName)
}

func TestPriorityRecorders(t *testing.T) {
	m := New()

	m.SetPriorityContracts("high", 7)
	m.SetPollLagByPriority("normal", -5)
	m.SetPollLagByPriority("normal", 3)

	assert.Equal(t, float64(7), m.sampleValue(t, priorityContractsName, map[string]string{"priority": "high"}))
	assert.Equal(t, float64(3), m.sampleValue(t, priorityLagName, map[string]string{"priority": "normal"}))
}

func TestRecordReorg(t *testing.T) {
	m := New()

	m.RecordReorg(321)

	assert.Equal(t, float64(1), m.sampleValue(t, reorgsName, nil))
	assert.Equal(t, float64(321), m.sampleValue(t, lastReorgLedgerName, nil))
}

func TestRecordThrottleLabelsByChannelType(t *testing.T) {
	m := New()

	m.RecordThrottle("slack")
	m.RecordThrottle("slack")
	m.RecordThrottle("email")

	assert.Equal(t, float64(2), m.sampleValue(t, throttlesName, map[string]string{"channel": "slack"}))
	assert.Equal(t, float64(1), m.sampleValue(t, throttlesName, map[string]string{"channel": "email"}))
}

func TestSetBreakerState(t *testing.T) {
	m := New()

	m.SetBreakerState("channel-1", "slack", "open")

	labels := map[string]string{"channel_id": "channel-1", "channel_type": "slack", "state": "open"}
	assert.Equal(t, float64(1), m.sampleValue(t, breakerStateName, labels))
	labels["state"] = "closed"
	assert.Equal(t, float64(0), m.sampleValue(t, breakerStateName, labels))
	labels["state"] = "half-open"
	assert.Equal(t, float64(0), m.sampleValue(t, breakerStateName, labels))
}

func TestRegisterStreamDropped(t *testing.T) {
	m := New()
	m.RegisterStreamDropped(func() uint64 { return 4 })
	m.RegisterStreamDropped(nil)

	assert.Equal(t, float64(4), m.sampleValue(t, streamDroppedName, nil))
}

func TestMiddlewareRecordsHTTPDuration(t *testing.T) {
	m := New()
	previousRoutePattern := RoutePattern
	RoutePattern = func(*http.Request) string { return "/health" }
	t.Cleanup(func() { RoutePattern = previousRoutePattern })
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health", nil)

	handler.ServeHTTP(response, request)

	assert.Equal(t, http.StatusCreated, response.Code)
	histogram := m.sampleHistogram(t, httpDurationName, map[string]string{
		"route": "/health", "method": http.MethodGet, "status": "201",
	})
	assert.Equal(t, uint64(1), histogram.GetSampleCount())
}

// TestRecordStoreReadLabelsByPool checks that a routed read is attributable:
// the pool label is what tells an operator whether routing is actually sending
// reads to the replica, and an unlabelled counter would make the metric
// useless for that one job.
func TestRecordStoreReadLabelsByPool(t *testing.T) {
	m := New()

	m.RecordStoreRead(poolNamePrimary)
	m.RecordStoreRead(poolNamePrimary)
	m.RecordStoreRead(poolNameReplica)

	assert.Equal(t, float64(2), m.sampleValue(t, storeReadsName, map[string]string{"pool": poolNamePrimary}))
	assert.Equal(t, float64(1), m.sampleValue(t, storeReadsName, map[string]string{"pool": poolNameReplica}))
}

func TestRecordReplicaFallbackCounts(t *testing.T) {
	m := New()

	m.RecordReplicaFallback()
	m.RecordReplicaFallback()
	m.RecordReplicaFallback()

	assert.Equal(t, float64(3), m.sampleValue(t, storeFallbacksName, nil))
}

// TestSetReplicaEnabledTracksBothDirections covers the gauge an operator reads
// to confirm the replica URL took effect, in both directions: it has to go
// back to 0 when routing is off, or a deployment that removed the replica
// would keep reporting one.
func TestSetReplicaEnabledTracksBothDirections(t *testing.T) {
	m := New()

	require.Equal(t, float64(0), m.sampleValue(t, replicaEnabledName, nil), "a fresh process reports no replica")

	m.SetReplicaEnabled(true)
	assert.Equal(t, float64(1), m.sampleValue(t, replicaEnabledName, nil))

	m.SetReplicaEnabled(false)
	assert.Equal(t, float64(0), m.sampleValue(t, replicaEnabledName, nil))
}

// TestReadMetricsAreNilSafe pins the package contract that a nil *Metrics
// records nothing rather than panicking. The store holds a *Metrics it was
// handed without a nil check, so this is load-bearing for every embedder and
// every test that constructs a store with no instrumentation.
func TestReadMetricsAreNilSafe(t *testing.T) {
	var m *Metrics

	assert.NotPanics(t, func() {
		m.RecordStoreRead(poolNamePrimary)
		m.RecordStoreRead(poolNameReplica)
		m.RecordReplicaFallback()
		m.SetReplicaEnabled(true)
		m.SetReplicaEnabled(false)
	})
}

// sampleValue returns the current value of the named metric, read out of the
// instance's own registry — what a scrape would see. It reads the gatherer
// directly rather than taking on prometheus/testutil (and the godebug module
// it pulls in) for a handful of numbers.
func (m *Metrics) sampleValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.registry.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, sample := range family.GetMetric() {
			if !hasLabels(sample, labels) {
				continue
			}
			if c := sample.GetCounter(); c != nil {
				return c.GetValue()
			}
			if g := sample.GetGauge(); g != nil {
				return g.GetValue()
			}
			t.Fatalf("%s is neither a counter nor a gauge", name)
		}
	}
	t.Fatalf("no sample for %s with labels %v", name, labels)
	return 0
}

func (m *Metrics) sampleHistogram(t *testing.T, name string, labels map[string]string) *dto.Histogram {
	t.Helper()
	families, err := m.registry.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() == name {
			for _, sample := range family.GetMetric() {
				if !hasLabels(sample, labels) {
					continue
				}
				histogram := sample.GetHistogram()
				require.NotNil(t, histogram)
				return histogram
			}
		}
	}
	t.Fatalf("no histogram for %s with labels %v", name, labels)
	return nil
}

// hasLabels reports whether sample carries exactly the wanted label pairs.
// Labels are compared as a set because prometheus does not promise an order.
func hasLabels(sample *dto.Metric, want map[string]string) bool {
	for _, pair := range sample.GetLabel() {
		if _, asked := want[pair.GetName()]; !asked {
			// Every ingest metric carries the network label, and on a
			// single-network instance its value is the empty string. A test
			// that does not name it is asking about that instance, so an
			// empty network is not a distinguishing label. Any other
			// unasked-for label still fails: it would mean the metric gained
			// a dimension the test has not been reviewed for.
			if pair.GetName() == networkLabel && pair.GetValue() == "" {
				continue
			}
			return false
		}
		if want[pair.GetName()] != pair.GetValue() {
			return false
		}
	}
	// Every label the caller named has to be present, so a typo in a test is
	// still a failure rather than a match against the first sample.
	for name := range want {
		found := false
		for _, pair := range sample.GetLabel() {
			if pair.GetName() == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
