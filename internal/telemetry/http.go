package telemetry

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// propagator is W3C trace context, set explicitly rather than read from the
// global so propagation works the same whether or not Setup installed a
// provider.
var propagator = propagation.TraceContext{}

// HTTP span attribute keys, following the OpenTelemetry semantic
// conventions. Only bounded values go in them: the method, the route
// pattern and the status code. Never the URL, path or query — a raw path
// carries ids (unbounded cardinality) and a channel URL carries credentials.
const (
	AttrHTTPMethod = "http.request.method"
	AttrHTTPRoute  = "http.route"
	AttrHTTPStatus = "http.response.status_code"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Middleware traces inbound HTTP requests. It continues a caller's trace
// from the W3C traceparent header, carries the request id (it must run
// after reqid.Middleware), and names the span by chi's route pattern
// ("GET /api/v1/alerts/{id}"), never the raw path, so span names stay
// bounded. A nil Provider passes requests straight through.
func (p *Provider) Middleware(next http.Handler) http.Handler {
	if p == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := p.WithRequestID(ctx, "HTTP "+r.Method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String(AttrHTTPMethod, r.Method)),
		)
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(ctx))

		route := ""
		if rc := chi.RouteContext(r.Context()); rc != nil {
			route = rc.RoutePattern()
		}
		if route == "" {
			route = "unmatched"
		}
		span.SetName(r.Method + " " + route)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		span.SetAttributes(attribute.String(AttrHTTPRoute, route), attribute.Int(AttrHTTPStatus, status))
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	})
}

// Transport traces outbound HTTP calls and propagates the trace to the
// callee in the traceparent header, so a channel endpoint that is itself
// traced continues the delivery's trace.
//
// The span records only the method and the status code. The request URL is
// deliberately absent: for a webhook, Slack or PagerDuty channel the URL
// (and often its query) is the credential, and the standing rule is that
// channel configuration never reaches a span. A nil Provider returns base
// unchanged.
func (p *Provider) Transport(base http.RoundTripper) http.RoundTripper {
	if p == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &tracingTransport{p: p, base: base}
}

type tracingTransport struct {
	p    *Provider
	base http.RoundTripper
}

func (t *tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := t.p.Tracer.Start(req.Context(), "HTTP "+req.Method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String(AttrHTTPMethod, req.Method)),
	)
	defer span.End()

	// Clone so the caller's request (and its headers) are left untouched.
	out := req.Clone(ctx)
	propagator.Inject(ctx, propagation.HeaderCarrier(out.Header))
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		span.SetStatus(codes.Error, "request failed")
		return resp, err
	}
	span.SetAttributes(attribute.Int(AttrHTTPStatus, resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}
	return resp, nil
}
