package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/reqid"
)

func recorded(t *testing.T) (*Provider, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	p := New(tp.Tracer("test"))
	p.enabled = true
	return p, rec
}

func spanNamed(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func attr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestStartLinkedIsANewRootLinkedToTheCause(t *testing.T) {
	p, rec := recorded(t)
	ctx, cause := p.Tracer.Start(context.Background(), "detect")
	_, linked := p.StartLinked(ctx, "deliver", trace.SpanContextFromContext(ctx))
	linked.End()
	cause.End()

	got := spanNamed(rec.Ended(), "deliver")
	require.NotNil(t, got)
	assert.False(t, got.Parent().IsValid(), "a linked span is a new root")
	assert.NotEqual(t, cause.SpanContext().TraceID(), got.SpanContext().TraceID())
	require.Len(t, got.Links(), 1)
	assert.Equal(t, cause.SpanContext().SpanID(), got.Links()[0].SpanContext.SpanID())
	_, ok := attr(got, AttrRequestID)
	assert.True(t, ok, "the request id rides on linked spans too")

	// An invalid cause starts a plain root with no link.
	_, plain := p.StartLinked(context.Background(), "orphan", trace.SpanContext{})
	plain.End()
	assert.Empty(t, spanNamed(rec.Ended(), "orphan").Links())
}

func TestAlertLinksAreBounded(t *testing.T) {
	p, _ := recorded(t)
	links := NewAlertLinks(2)
	ctxs := make([]trace.SpanContext, 3)
	for i := range ctxs {
		_, s := p.Tracer.Start(context.Background(), "a")
		ctxs[i] = s.SpanContext()
		s.End()
		links.Remember(int64(i+1), ctxs[i])
	}
	_, ok := links.Lookup(1)
	assert.False(t, ok, "the oldest alert is forgotten first")
	got, ok := links.Lookup(3)
	require.True(t, ok)
	assert.Equal(t, ctxs[2], got)

	links.Remember(9, trace.SpanContext{})
	_, ok = links.Lookup(9)
	assert.False(t, ok, "an invalid span context is not remembered")

	var none *AlertLinks
	none.Remember(1, ctxs[0])
	_, ok = none.Lookup(1)
	assert.False(t, ok, "a nil store is safe")
}

func TestMiddlewareContinuesTheCallersTraceAndNamesByRoute(t *testing.T) {
	p, rec := recorded(t)
	r := chi.NewRouter()
	r.Use(reqid.Middleware, p.Middleware)
	r.Get("/api/v1/alerts/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	// A caller's trace arrives in traceparent.
	ctx, caller := p.Tracer.Start(context.Background(), "caller")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts/12345?token=secret", nil)
	propagator.Inject(ctx, httpHeader(req))
	caller.End()
	r.ServeHTTP(httptest.NewRecorder(), req)

	got := spanNamed(rec.Ended(), "GET /api/v1/alerts/{id}")
	require.NotNil(t, got, "the span is named by the route pattern, not the raw path")
	assert.Equal(t, trace.SpanKindServer, got.SpanKind())
	assert.Equal(t, caller.SpanContext().TraceID(), got.SpanContext().TraceID(), "the caller's trace continues")
	assert.Equal(t, caller.SpanContext().SpanID(), got.Parent().SpanID())
	status, _ := attr(got, AttrHTTPStatus)
	assert.Equal(t, int64(http.StatusTeapot), status.AsInt64())
	id, ok := attr(got, AttrRequestID)
	require.True(t, ok)
	assert.NotEmpty(t, id.AsString())
	for _, kv := range got.Attributes() {
		assert.NotContains(t, kv.Value.String(), "12345", "no raw path ids in attributes")
		assert.NotContains(t, kv.Value.String(), "secret", "no query values in attributes")
	}
}

func TestMiddlewareNamesUnmatchedRoutesBoundedly(t *testing.T) {
	p, rec := recorded(t)
	r := chi.NewRouter()
	r.Use(p.Middleware)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/no/such/path/42", nil))
	names := []string{}
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	require.Len(t, names, 1)
	assert.NotContains(t, names[0], "42", "an unmatched path never becomes a span name")
}

func TestTransportPropagatesWithoutRecordingTheURL(t *testing.T) {
	p, rec := recorded(t)
	var gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := &http.Client{Transport: p.Transport(nil)}
	ctx, parent := p.Tracer.Start(context.Background(), "notify.deliver")
	webhook := srv.URL + "/hooks/T000/B000/XXXXsecretXXXX?token=abc"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	parent.End()

	assert.Contains(t, gotTraceparent, parent.SpanContext().TraceID().String(),
		"the endpoint receives the delivery's trace")
	assert.Empty(t, req.Header.Get("traceparent"), "the caller's request is not mutated")

	got := spanNamed(rec.Ended(), "HTTP POST")
	require.NotNil(t, got)
	assert.Equal(t, trace.SpanKindClient, got.SpanKind())
	status, _ := attr(got, AttrHTTPStatus)
	assert.Equal(t, int64(http.StatusAccepted), status.AsInt64())
	for _, kv := range got.Attributes() {
		v := kv.Value.String()
		assert.NotContains(t, v, "secret", "the channel URL is never an attribute")
		assert.NotContains(t, v, srv.URL)
	}
}

func TestNilProviderPassesThrough(t *testing.T) {
	var p *Provider
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	rr := httptest.NewRecorder()
	p.Middleware(h).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, 204, rr.Code)
	assert.Equal(t, http.DefaultTransport, p.Transport(http.DefaultTransport))
}

func TestStatementIdentityHasNoParameters(t *testing.T) {
	op, id := StatementIdentity("SELECT id, payload FROM alerts\n  WHERE workspace_id = $1 AND id = $2")
	assert.Equal(t, "SELECT", op)
	assert.Len(t, id, 12)
	_, same := StatementIdentity("SELECT id, payload FROM alerts WHERE workspace_id = $1 AND id = $2")
	assert.Equal(t, id, same, "whitespace does not change the identity")
	_, other := StatementIdentity("SELECT id FROM monitors WHERE id = $1")
	assert.NotEqual(t, id, other)
	op, _ = StatementIdentity("insert into alerts (x) values ($1)")
	assert.Equal(t, "INSERT", op)
	op, _ = StatementIdentity("vacuum")
	assert.Equal(t, "QUERY", op)
}

func TestQueryTracerRecordsIdentityOnly(t *testing.T) {
	p, rec := recorded(t)
	qt := p.NewQueryTracer()
	ctx := qt.TraceQueryStart(context.Background(), nil, pgxStart("UPDATE alerts SET status = $1 WHERE id = $2", "resolved", 42))
	qt.TraceQueryEnd(ctx, nil, pgxEnd(nil))

	got := spanNamed(rec.Ended(), "db.update")
	require.NotNil(t, got)
	op, _ := attr(got, AttrDBOperation)
	assert.Equal(t, "UPDATE", op.AsString())
	for _, kv := range got.Attributes() {
		assert.NotContains(t, kv.Value.String(), "resolved", "parameter values never reach a span")
		assert.NotContains(t, kv.Value.String(), "SET status", "the SQL text is not recorded")
	}
}
