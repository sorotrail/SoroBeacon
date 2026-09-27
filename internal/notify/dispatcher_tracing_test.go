package notify

// Tracing tests for the dispatcher. In production Dispatch is handed the
// poller's alert-span context, so the notify.deliver span lands under the
// alert's span on the poll cycle's trace; these tests re-create that shape
// with an in-memory exporter and pin the attributes and the no-secrets
// rule on the span itself.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
)

// tracedDispatcher builds a Dispatcher over the mock notifier wired to an
// in-memory span exporter, returning it plus the recorder.
func tracedDispatcher(t *testing.T, st *fakeDispatchStore, n Notifier) (*Dispatcher, *tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
	exp := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(exp))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	tel := telemetry.New(tp.Tracer("test"))

	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return n, nil })
	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler)).WithTelemetry(tel)
	d.BaseBackoff = time.Millisecond
	return d, exp, tp.Tracer("test-parent")
}

func TestDeliverSpanIsChildOfAlertContext(t *testing.T) {
	// The dispatcher must not start roots: whatever ctx Dispatch receives
	// (in production the poller's alert span) is the parent.
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{}
	d, exp, tracer := tracedDispatcher(t, st, n)

	// Hand Dispatch a context carrying an alert-like parent span, exactly
	// as the poller's fireAlert does.
	ctx, alertSpan := tracer.Start(context.Background(), "poller.create_alert")
	d.Dispatch(ctx, Alert{ID: 10, MonitorID: 2})
	alertSpan.End()

	spans := exp.Ended()
	require.Len(t, spans, 2, "the fake alert parent and the delivery span")
	deliver := findSpan(spans, "notify.deliver")
	require.NotNil(t, deliver)
	assert.Equal(t, alertSpan.SpanContext().SpanID(), deliver.Parent().SpanID(),
		"the delivery span must be a child of the alert's span, never a root")
}

// findSpan picks the first span with the given name from a batch of ended spans.
func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func TestDeliverSpanCarriesChannelIDNotConfig(t *testing.T) {
	// Channel identity is the row id and static type. The config — webhook
	// URLs, bot tokens — must appear nowhere on the span, and the notifier
	// error (kept secret-free by convention) may appear as an event only.
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(3)}}
	n := &mockNotifier{failures: 1}
	d, exp, _ := tracedDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 11, MonitorID: 2})

	spans := exp.Ended()
	require.Len(t, spans, 1)
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "11", attrs[telemetry.AttrAlertID])
	assert.Equal(t, "3", attrs[telemetry.AttrChannelID])
	assert.Equal(t, "mock", attrs[telemetry.AttrChannelType])
	for key := range attrs {
		assert.NotContains(t, key, "config")
		assert.NotContains(t, key, "webhook")
		assert.NotContains(t, key, "token")
	}
}

func TestDeliverSpanRecordsRequestID(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{{ID: 1, Type: "mock", Enabled: true}}}
	n := &mockNotifier{}
	d, exp, _ := tracedDispatcher(t, st, n)

	ctx := reqid.WithContext(context.Background(), "req-77")
	d.Dispatch(ctx, Alert{ID: 12, MonitorID: 2})

	spans := exp.Ended()
	require.Len(t, spans, 1)
	var rid string
	for _, kv := range spans[0].Attributes() {
		if string(kv.Key) == telemetry.AttrRequestID {
			rid = kv.Value.String()
		}
	}
	assert.Equal(t, "req-77", rid, "delivery spans must carry the request id so logs join traces")
}

func TestDeliverSpanRetriesAreOneSpan(t *testing.T) {
	// Retries are attempts inside one channel's span, not spans of their
	// own: the span shows the whole wall-clock cost of reaching the channel.
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{failures: 2}
	d, exp, _ := tracedDispatcher(t, st, n)

	d.Dispatch(context.Background(), Alert{ID: 13, MonitorID: 2})
	require.Equal(t, 3, n.calls)

	spans := exp.Ended()
	require.Len(t, spans, 1, "retries live inside one span per channel")
	events := spans[0].Events()
	assert.GreaterOrEqual(t, len(events), 2, "each failed attempt is recorded as an error event")
}

func TestDeliverSpanBadConfigMarksError(t *testing.T) {
	st := &fakeDispatchStore{channels: []store.Channel{{ID: 5, Type: "nope", Config: json.RawMessage(`{}`), Enabled: true}}}
	d, exp, _ := tracedDispatcher(t, st, &mockNotifier{})

	d.Dispatch(context.Background(), Alert{ID: 14, MonitorID: 2})

	spans := exp.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code,
		"a notifier construction failure must mark the span")
}

func TestRetrySpanUsesRequestContext(t *testing.T) {
	st := &fakeDispatchStore{}
	n := &mockNotifier{}
	d, exp, _ := tracedDispatcher(t, st, n)

	ctx := reqid.WithContext(context.Background(), "req-retry")
	d.Retry(ctx, Alert{ID: 20, MonitorID: 2}, mockChannel(3))

	spans := exp.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "notify.retry", spans[0].Name())
	var rid string
	for _, kv := range spans[0].Attributes() {
		if string(kv.Key) == telemetry.AttrRequestID {
			rid = kv.Value.String()
		}
	}
	assert.Equal(t, "req-retry", rid)
}

func TestDispatcherWithoutTelemetryNilSafe(t *testing.T) {
	// Every existing dispatcher test runs without telemetry; pin that a nil
	// provider changes nothing and panics nowhere.
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return &mockNotifier{}, nil })
	d := NewDispatcher(st, f, slog.New(slog.DiscardHandler))
	d.telemetry = nil
	d.BaseBackoff = time.Millisecond

	d.Dispatch(context.Background(), Alert{ID: 15, MonitorID: 2})
	assert.Len(t, st.attempts, 1)
}
