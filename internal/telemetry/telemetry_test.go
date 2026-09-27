// Package telemetry owns SoroBeacon's OpenTelemetry tracing setup; this
// file holds the tracing smoke tests for the package itself plus the
// in-memory exporter helper other packages' tests use to assert on span
// trees without a collector.
package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sorotrail/sorobeacon/internal/reqid"
)

// recorder is an in-memory span exporter + provider pair for tests: spans
// started through it land in Ended() instead of the wire. Keep it here so
// every package's tracing tests share one shape.
type recorder struct {
	exp *tracetest.SpanRecorder
	p   *Provider
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	exp := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(exp))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	return &recorder{
		exp: exp,
		p:   &Provider{Tracer: tp.Tracer("test"), enabled: true},
	}
}

func TestSetupDisabledIsNoop(t *testing.T) {
	p, err := Setup(context.Background(), Config{})
	require.NoError(t, err)
	assert.False(t, p.Enabled(), "no endpoint must mean tracing entirely off")

	// Spans from a disabled provider are no-ops: they carry no trace
	// context, so nothing can leak onto the wire.
	ctx, span := p.StartSpan(context.Background(), "x")
	assert.NotNil(t, ctx)
	assert.False(t, span.SpanContext().IsValid(), "disabled tracing must produce non-recording spans")
	span.End()
	assert.NoError(t, p.Shutdown(context.Background()), "shutdown on a disabled provider is a no-op")
	// A nil provider is safe too (call sites hold *Provider without checks).
	var nilP *Provider
	assert.False(t, nilP.Enabled())
	assert.NoError(t, nilP.Shutdown(context.Background()))
}

func TestSetupEnabledStartsRealProvider(t *testing.T) {
	// The exporter never fires a request in this test: the provider is
	// shut down before the batcher's first flush, and the endpoint is a
	// closed port anyway.
	p, err := Setup(context.Background(), Config{Endpoint: "http://127.0.0.1:1", SampleRate: 1})
	require.NoError(t, err)
	assert.True(t, p.Enabled())

	_, span := p.StartSpan(context.Background(), "x")
	assert.True(t, span.SpanContext().IsValid(), "enabled tracing must record spans")
	span.End()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	assert.NoError(t, p.Shutdown(shutdownCtx))
}

func TestStartSpanCarriesParentFromContext(t *testing.T) {
	r := newRecorder(t)
	ctx, root := r.p.StartSpan(context.Background(), "root")
	_, child := r.p.StartSpan(ctx, "child")
	child.End()
	root.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 2)
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	assert.Equal(t, byName["root"].SpanContext().SpanID(), byName["child"].Parent().SpanID(),
		"the child must be parented to the root, not be a second root")
}

func TestWithRequestIDCarriesReqid(t *testing.T) {
	r := newRecorder(t)
	ctx := reqid.WithContext(context.Background(), "abc123")
	_, span := r.p.WithRequestID(ctx, "stage")
	span.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 1)
	got, ok := attributeValue(spans[0].Attributes(), AttrRequestID)
	assert.True(t, ok, "request_id attribute must be set")
	assert.Equal(t, "abc123", got)
}

func TestWithRequestIDGeneratesWhenMissing(t *testing.T) {
	r := newRecorder(t)
	_, span := r.p.WithRequestID(context.Background(), "stage")
	span.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 1)
	got, ok := attributeValue(spans[0].Attributes(), AttrRequestID)
	assert.True(t, ok)
	assert.NotEmpty(t, got, "a stage with no request id still gets one, once per stage")
}

func TestSetAttrsSkipsUnsupported(t *testing.T) {
	r := newRecorder(t)
	_, span := r.p.StartSpan(context.Background(), "s")
	SetAttrs(span,
		AttrAlertID, int64(7),
		"count", 3,
		"flag", true,
		AttrChannelType, "discord",
		// A map (i.e. channel config) must be skipped, not stringified.
		"config", map[string]any{"webhook_url": "https://hooks.example/x"},
	)
	span.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 1)
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "7", attrs[AttrAlertID])
	assert.Equal(t, "3", attrs["count"])
	assert.Equal(t, "true", attrs["flag"])
	assert.Equal(t, "discord", attrs[AttrChannelType])
	assert.NotContains(t, attrs, "config")
	assert.NotContains(t, attrs, "webhook_url")
}

func TestSetAttrsNeverRecordsChannelConfig(t *testing.T) {
	// The hard rule from the issue: no span attribute may contain channel
	// config, tokens or webhook URLs. SetAttrs's supported types cannot
	// smuggle a struct or map through, and this pins that.
	r := newRecorder(t)
	_, span := r.p.StartSpan(context.Background(), "deliver")
	SetAttrs(span,
		AttrChannelID, int64(42),
		"webhook_url", nil, // unsupported: skipped
		"bot_token", struct{ Secret string }{Secret: "hunter2"}, // skipped
	)
	span.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 1)
	for _, kv := range spans[0].Attributes() {
		assert.NotEqual(t, "webhook_url", string(kv.Key))
		assert.NotEqual(t, "bot_token", string(kv.Key))
	}
}

func TestRecordErrorMarksSpanFailed(t *testing.T) {
	r := newRecorder(t)
	_, span := r.p.StartSpan(context.Background(), "s")
	RecordError(span, assert.AnError)
	span.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, 1, len(spans[0].Events()), "the error must be recorded as an event")
	assert.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestTraceIDHelperIsHex32(t *testing.T) {
	id := TraceID()
	assert.Len(t, id, 32)
	for _, c := range id {
		assert.Contains(t, "0123456789abcdef", string(c))
	}
}

// --- helpers shared by the tests above ---

func attributeValue(attrs []attribute.KeyValue, key string) (string, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}
