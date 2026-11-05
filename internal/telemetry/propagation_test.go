package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/sorotrail/sorobeacon/internal/reqid"
)

func TestInjectAndExtractHTTPRoundTripsW3CTraceParent(t *testing.T) {
	r := newRecorder(t)

	ctx, clientSpan := r.p.StartSpan(context.Background(), "notify.deliver")
	headers := make(http.Header)
	InjectHTTP(ctx, headers)
	clientSpan.End()

	traceparent := headers.Get("Traceparent")
	require.NotEmpty(t, traceparent, "InjectHTTP must populate W3C Traceparent header for active span")
	assert.Contains(t, traceparent, clientSpan.SpanContext().TraceID().String())
	assert.Contains(t, traceparent, clientSpan.SpanContext().SpanID().String())

	// Extract on the receiving side and start a child span.
	extractedCtx := ExtractHTTP(context.Background(), headers)
	_, serverSpan := r.p.StartSpan(extractedCtx, "federation.ingest")
	serverSpan.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 2)
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}

	assert.Equal(t,
		byName["notify.deliver"].SpanContext().TraceID(),
		byName["federation.ingest"].SpanContext().TraceID(),
		"extracted W3C traceparent must preserve the upstream trace ID end to end",
	)
	assert.Equal(t,
		byName["notify.deliver"].SpanContext().SpanID(),
		byName["federation.ingest"].Parent().SpanID(),
		"downstream span must be parented to the injected client span",
	)
}

func TestInjectHTTPNoopWhenDisabledOrNil(t *testing.T) {
	headers := make(http.Header)
	InjectHTTP(context.Background(), headers)
	assert.Empty(t, headers.Get("Traceparent"), "no active span must not emit a bogus traceparent header")

	InjectHTTP(nil, headers)
	InjectHTTP(context.Background(), nil)
	assert.NotNil(t, ExtractHTTP(nil, nil))
}

func TestCarrierRoundTripAndStartLinkedSpanAcrossAsyncBoundary(t *testing.T) {
	r := newRecorder(t)

	// Simulate the poll cycle creating an alert span with a request_id.
	originCtx := reqid.WithContext(context.Background(), "req-poll-42")
	originCtx, alertSpan := r.p.WithRequestID(originCtx, "poller.create_alert")
	carrier := CarrierFromContext(originCtx)
	alertSpan.End()

	require.NotEmpty(t, carrier["traceparent"])
	assert.Equal(t, "req-poll-42", carrier[AttrRequestID])

	// Simulate an asynchronous escalation worker or delayed retry timer waking
	// up in a separate goroutine with a fresh background context, restoring the
	// carrier, and starting a linked span.
	restoredOrigin := ContextWithCarrier(context.Background(), carrier)
	_, escSpan := r.p.StartLinkedSpan(context.Background(), restoredOrigin, "escalation.step")
	escSpan.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 2)
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}

	esc := byName["escalation.step"]
	require.NotNil(t, esc)
	require.Len(t, esc.Links(), 1, "async escalation span must carry a Span Link to the originating alert span")
	assert.Equal(t, alertSpan.SpanContext().TraceID(), esc.Links()[0].SpanContext.TraceID())
	assert.Equal(t, alertSpan.SpanContext().SpanID(), esc.Links()[0].SpanContext.SpanID())

	rid, ok := attributeValue(esc.Attributes(), AttrRequestID)
	require.True(t, ok)
	assert.Equal(t, "req-poll-42", rid, "linked span must preserve the originating request_id")
}

func TestStartDBSpanRecordsOperationWithoutQueryParameters(t *testing.T) {
	r := newRecorder(t)
	ctx := reqid.WithContext(context.Background(), "req-db-9")

	_, dbSpan := r.p.StartDBSpan(ctx, "list_monitors")
	dbSpan.End()

	spans := r.exp.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "store.list_monitors", spans[0].Name())

	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "postgresql", attrs[AttrDBSystem])
	assert.Equal(t, "list_monitors", attrs[AttrDBOperation])
	assert.Equal(t, "req-db-9", attrs[AttrRequestID])
	assert.Len(t, attrs, 3, "DB span must record only db.system, db.operation, and request_id — never query parameters")
}

func TestHTTPMiddlewareExtractsUpstreamTraceAndEchoesTraceIDHeader(t *testing.T) {
	r := newRecorder(t)

	// Start an upstream caller span and inject its W3C traceparent into the request.
	upstreamCtx, upstreamSpan := r.p.StartSpan(context.Background(), "upstream.caller")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors/42?secret=should-not-appear", nil)
	InjectHTTP(upstreamCtx, req.Header)
	upstreamSpan.End()

	mw := r.p.HTTPMiddleware(func(*http.Request) string {
		return "/api/v1/monitors/{id}"
	})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, httpReq *http.Request) {
		_, dbSpan := r.p.StartDBSpan(httpReq.Context(), "get_monitor")
		dbSpan.End()
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, upstreamSpan.SpanContext().TraceID().String(), rec.Header().Get(TraceIDHeader),
		"X-Trace-ID response header must match the active trace ID")

	spans := r.exp.Ended()
	require.Len(t, spans, 3)
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}

	httpSpan := byName["http.request"]
	dbSpan := byName["store.get_monitor"]
	require.NotNil(t, httpSpan)
	require.NotNil(t, dbSpan)

	assert.Equal(t, upstreamSpan.SpanContext().TraceID(), httpSpan.SpanContext().TraceID())
	assert.Equal(t, upstreamSpan.SpanContext().SpanID(), httpSpan.Parent().SpanID())
	assert.Equal(t, httpSpan.SpanContext().SpanID(), dbSpan.Parent().SpanID())

	route, ok := attributeValue(httpSpan.Attributes(), AttrHTTPRoute)
	require.True(t, ok)
	assert.Equal(t, "/api/v1/monitors/{id}", route, "HTTP span must record matched route pattern, never raw URL query")
}

func TestHTTPMiddlewareMarks5xxAsError(t *testing.T) {
	r := newRecorder(t)
	mw := r.p.HTTPMiddleware(nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler.ServeHTTP(rec, req)

	spans := r.exp.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
}
