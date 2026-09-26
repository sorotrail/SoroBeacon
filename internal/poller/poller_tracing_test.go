package poller

// Tracing tests: one poll cycle must become one trace. The in-memory
// exporter records the span tree, so these tests assert on real parent /
// child links with no collector running. The first test is the issue's
// acceptance criterion: a delivery span must be a child of the alert's
// span, never a root, and it fails if that propagation regresses.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
)

// recordingNotifier is a Notifier whose factory always succeeds, so the
// dispatcher's notify.deliver span exists without any network.
type recordingNotifier struct{}

func (recordingNotifier) Send(context.Context, notify.Alert) error { return nil }

// newTracedPoller wires the full pipeline — poller, registry, dispatcher —
// to one in-memory exporter, mirroring the wiring in cmd/sorobeacon. It
// returns the poller and the recorder to read finished spans from.
func newTracedPoller(t *testing.T, rpc *fakeRPC, st *fakeStore) (*Poller, *tracetest.SpanRecorder) {
	t.Helper()
	exp := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(exp))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	tel := telemetry.New(tp.Tracer("test"))

	p := newTestPoller(rpc, st, nil)
	p.telemetry = tel
	p.registry.WithTelemetry(tel)

	factory := notify.DefaultFactory()
	factory.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return recordingNotifier{}, nil })
	d := notify.NewDispatcher(st, factory, p.log).WithTelemetry(tel)
	p.dispatch = d
	return p, exp
}

func TestPollCreatesOneTraceDeliveryUnderAlertSpan(t *testing.T) {
	// The acceptance criterion: poll cycle -> fetch -> rules -> alert ->
	// delivery, all one trace. Break the context propagation in fireAlert
	// (pass context.Background() to Dispatch) and this fails.
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	// A channel must exist for the dispatcher to attempt deliveries.
	st.channels = []store.Channel{{ID: 7, Type: "mock", Enabled: true}}
	st.attached[1] = []int64{7}

	rpc := &fakeRPC{latest: 6000, responses: []*stellar.GetEventsResult{{
		Events:       []stellar.Event{transferEvent("ev-1", 5990, "1")},
		LatestLedger: 6000,
	}}}
	p, exp := newTracedPoller(t, rpc, st)
	require.NoError(t, p.Poll(context.Background()))
	require.Len(t, st.alerts, 1, "the seeded event must fire one alert")

	spans := exp.Ended()
	require.NotEmpty(t, spans)

	byName := map[string][]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = append(byName[s.Name()], s)
	}
	require.Contains(t, byName, "poller.poll")
	require.Contains(t, byName, "poller.fetch_events")
	require.Contains(t, byName, "rules.evaluate")
	require.Contains(t, byName, "poller.create_alert")
	require.Contains(t, byName, "notify.deliver")

	// The whole tree shares exactly one trace id.
	traceIDs := map[any]bool{}
	for _, s := range spans {
		traceIDs[s.SpanContext().TraceID()] = true
	}
	assert.Len(t, traceIDs, 1, "one poll cycle must produce exactly one trace")

	// Every delivery span is a child of the alert's span — never a root.
	for _, alert := range byName["poller.create_alert"] {
		for _, d := range byName["notify.deliver"] {
			assert.Equal(t, alert.SpanContext().SpanID(), d.Parent().SpanID(),
				"a delivery span must be a child of the alert's span, never a root")
		}
	}
	// The alert itself hangs off the cycle root.
	for _, root := range byName["poller.poll"] {
		for _, alert := range byName["poller.create_alert"] {
			assert.Equal(t, root.SpanContext().SpanID(), alert.Parent().SpanID(),
				"the alert span must be a child of the poll cycle span")
		}
	}
}

func TestPollSpanCarriesRequestIDAttribute(t *testing.T) {
	// The existing request id must appear as a span attribute, so logs and
	// traces join. The poller stages reuse the ambient id when the cycle
	// was kicked off under one (the API's poll-now path).
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)

	rpc := &fakeRPC{latest: 6000, responses: []*stellar.GetEventsResult{{
		Events:       []stellar.Event{transferEvent("ev-1", 5990, "1")},
		LatestLedger: 6000,
	}}}
	p, exp := newTracedPoller(t, rpc, st)

	// Simulate a poll-now HTTP request by planting a request id in ctx.
	ctx := reqid.WithContext(context.Background(), "req-42")
	require.NoError(t, p.Poll(ctx))

	found := false
	for _, s := range exp.Ended() {
		if s.Name() != "poller.poll" {
			continue
		}
		for _, kv := range s.Attributes() {
			if string(kv.Key) == telemetry.AttrRequestID {
				found = true
				assert.Equal(t, "req-42", kv.Value.AsString())
			}
		}
	}
	assert.True(t, found, "poller.poll must carry a request_id attribute")
}

func TestPollSpanRecordsScannedAndMatched(t *testing.T) {
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)

	rpc := &fakeRPC{latest: 6000, responses: []*stellar.GetEventsResult{{
		Events: []stellar.Event{
			transferEvent("ev-1", 5990, "1"),
			transferEvent("ev-2", 5990, "2"),
		},
		LatestLedger: 6000,
	}}}
	p, exp := newTracedPoller(t, rpc, st)
	require.NoError(t, p.Poll(context.Background()))

	for _, s := range exp.Ended() {
		if s.Name() != "poller.poll" {
			continue
		}
		attrs := map[string]string{}
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		assert.Equal(t, "2", attrs[telemetry.AttrEventsScanned])
		assert.Equal(t, "2", attrs[telemetry.AttrEventsMatched])
	}
}

func TestPollWithoutTelemetryStillWorks(t *testing.T) {
	// Nil-safe: every existing test constructs pollers without telemetry,
	// but pin it explicitly — a nil provider must not panic and must not
	// change poller behaviour.
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	rpc := &fakeRPC{latest: 6000, responses: []*stellar.GetEventsResult{{
		Events:       []stellar.Event{transferEvent("ev-1", 5990, "1")},
		LatestLedger: 6000,
	}}}
	p := newTestPoller(rpc, st, &fakeDispatcher{})
	p.telemetry = nil

	require.NoError(t, p.Poll(context.Background()))
	require.Len(t, st.alerts, 1)
}

func TestRegistryEvaluateSpanUnknownRuleTypeRecordsError(t *testing.T) {
	r := rules.NewRegistry()
	exp := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(exp))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	r.WithTelemetry(telemetry.New(tp.Tracer("test")))

	ev := &stellar.DecodedEvent{ID: "ev-1", ContractID: contractA}
	_, err := r.Evaluate(context.Background(), "nope", ev, nil)
	require.Error(t, err)

	var found bool
	for _, s := range exp.Ended() {
		if s.Name() == "rules.evaluate" {
			found = true
			assert.NotEqual(t, codes.Ok, s.Status().Code, "the failure must mark the span")
		}
	}
	assert.True(t, found, "unknown rule types still get a span, so the failure is visible")
}

func TestRegistryWithoutTelemetryNilSafe(t *testing.T) {
	r := rules.NewRegistry()
	ev := &stellar.DecodedEvent{ID: "ev-1", ContractID: contractA, Topics: []any{"transfer"}}
	params := json.RawMessage(`{"event_name": "transfer"}`)
	matched, err := r.Evaluate(context.Background(), rules.TypeEventEmitted, ev, params)
	require.NoError(t, err)
	assert.True(t, matched)
}
