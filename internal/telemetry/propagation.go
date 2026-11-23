package telemetry

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/reqid"
)

// Additional span attribute keys for HTTP, database, and poll-mode metadata.
const (
	AttrDBSystem    = "db.system"
	AttrDBOperation = "db.operation"
	AttrHTTPMethod  = "http.method"
	AttrHTTPRoute   = "http.route"
	AttrHTTPStatus  = "http.status_code"
	AttrPollMode    = "poll_mode"
	AttrNetwork     = "network"
	AttrLedgerFrom  = "ledger_from"
	AttrLedgerTo    = "ledger_to"
)

// TraceIDHeader is the response header emitted by HTTPMiddleware when a
// recording span is active on the request, allowing callers and operators to
// jump straight from an HTTP response to the trace in Tempo/Jaeger.
const TraceIDHeader = "X-Trace-ID"

var w3cPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
	propagation.Baggage{},
)

// Propagator returns SoroBeacon's W3C TraceContext + Baggage propagator.
func Propagator() propagation.TextMapPropagator {
	return w3cPropagator
}

// InjectHTTP writes the active span's W3C traceparent/tracestate headers into
// an outbound http.Header map. Safe to call when tracing is disabled or when
// ctx carries no valid span (nothing is written).
func InjectHTTP(ctx context.Context, h http.Header) {
	if ctx == nil || h == nil {
		return
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return
	}
	w3cPropagator.Inject(ctx, propagation.HeaderCarrier(h))
}

// ExtractHTTP reads W3C traceparent/tracestate headers from an inbound
// http.Header map and returns a context carrying the remote parent SpanContext.
func ExtractHTTP(ctx context.Context, h http.Header) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return ctx
	}
	return w3cPropagator.Extract(ctx, propagation.HeaderCarrier(h))
}

// CarrierFromContext serializes the active SpanContext and request_id from ctx
// into a flat string map suitable for passing across asynchronous queues,
// retry timers, or escalation workers.
func CarrierFromContext(ctx context.Context) map[string]string {
	carrier := make(map[string]string, 3)
	if ctx == nil {
		return carrier
	}
	if trace.SpanContextFromContext(ctx).IsValid() {
		w3cPropagator.Inject(ctx, propagation.MapCarrier(carrier))
	}
	if id := reqid.FromContext(ctx); id != "" {
		carrier[AttrRequestID] = id
	}
	return carrier
}

// ContextWithCarrier restores a remote SpanContext and request_id from a flat
// carrier map created by CarrierFromContext.
func ContextWithCarrier(ctx context.Context, carrier map[string]string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(carrier) == 0 {
		return ctx
	}
	ctx = w3cPropagator.Extract(ctx, propagation.MapCarrier(carrier))
	if id := carrier[AttrRequestID]; id != "" && reqid.FromContext(ctx) == "" {
		ctx = reqid.WithContext(ctx, id)
	}
	return ctx
}

// StartLinkedSpan starts a span for asynchronous follow-up work (such as
// delayed retries, digest flushes, or escalation ticks) with a Span Link back
// to originCtx's span context. Linking preserves causal navigation in both
// directions without stretching the originating poll cycle's root span across
// minutes or hours of retry backoff.
func (p *Provider) StartLinkedSpan(ctx, originCtx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if p == nil || p.Tracer == nil {
		return ctx, NoopSpan()
	}
	if originCtx != nil {
		if originSC := trace.SpanContextFromContext(originCtx); originSC.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSC}))
		}
		if reqid.FromContext(ctx) == "" {
			if id := reqid.FromContext(originCtx); id != "" {
				ctx = reqid.WithContext(ctx, id)
			}
		}
	}
	return p.WithRequestID(ctx, name, opts...)
}

// StartDBSpan starts a child span for a PostgreSQL store operation. To prevent
// query parameters, webhook URLs, or channel credentials from ever leaking
// into span attributes, only the static operation name (e.g. "create_alert",
// "list_monitors") and ambient request_id are recorded.
func (p *Provider) StartDBSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	if p == nil || p.Tracer == nil {
		return ctx, NoopSpan()
	}
	cleanOp := strings.TrimSpace(operation)
	if cleanOp == "" {
		cleanOp = "query"
	}
	spanName := cleanOp
	if !strings.HasPrefix(spanName, "store.") {
		spanName = "store." + spanName
	}
	return p.WithRequestID(ctx, spanName,
		trace.WithAttributes(
			attribute.String(AttrDBSystem, "postgresql"),
			attribute.String(AttrDBOperation, cleanOp),
		),
	)
}

type traceStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *traceStatusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// HTTPMiddleware extracts inbound W3C trace context headers, starts an
// http.request span carrying the matched route pattern (never raw query
// parameters), echoes X-Trace-ID on the response when recording, and marks 5xx
// responses as span errors.
func (p *Provider) HTTPMiddleware(routePattern func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p == nil || p.Tracer == nil {
				next.ServeHTTP(w, r)
				return
			}
			ctx := ExtractHTTP(r.Context(), r.Header)
			ctx, span := p.WithRequestID(ctx, "http.request",
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attribute.String(AttrHTTPMethod, r.Method)),
			)
			defer span.End()

			if traceID := SpanID(span); traceID != "" {
				w.Header().Set(TraceIDHeader, traceID)
			}

			rec := &traceStatusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			route := r.URL.Path
			if routePattern != nil {
				if pat := routePattern(r); pat != "" {
					route = pat
				}
			}
			span.SetAttributes(
				attribute.String(AttrHTTPRoute, route),
				attribute.Int(AttrHTTPStatus, rec.status),
			)
			if rec.status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}
