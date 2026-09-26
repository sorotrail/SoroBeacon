// Package telemetry owns SoroBeacon's OpenTelemetry tracing setup.
//
// Tracing is entirely opt-in: with no OTLP endpoint configured, Setup
// returns a no-op tracer, spans cost a context value and nothing is
// exported. When an endpoint is set, one OTLP/HTTP exporter ships spans to
// the collector, and Shutdown flushes whatever is still pending — call it
// on the way down, or the last moments of a deployment are exactly the
// spans an operator debugging that deployment wants.
//
// The wiring rule for call sites: start a span with Provider.StartSpan (or
// the Tracer directly), do the work, record the error with
// span.RecordError and span.SetStatus, and let the context carry the trace
// onward. Boundary spans already exist for the poll cycle, event decode,
// rule evaluation, alert persistence and each channel delivery; a delivery
// span must be a child of the alert's span, never a root — that parent
// chain is the whole point, and the poller test pins it.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceName is the default service.name resource attribute. Operators
// override it per deployment with OTLP_SERVICE_NAME.
const ServiceName = "sorobeacon"

// Config selects the OTLP exporter. The zero value disables tracing
// completely, so a deployment that never heard of OTLP gets exactly the
// binary that existed before — same startup, same overhead.
type Config struct {
	// Endpoint is the OTLP/HTTP base URL (OTLP_ENDPOINT, e.g.
	// http://localhost:4318). Empty disables tracing.
	Endpoint string
	// ServiceName is reported as the service.name resource attribute;
	// empty falls back to the default.
	ServiceName string
	// SampleRate is the fraction of traces kept, in [0,1]
	// (OTLP_SAMPLE_RATE). 1 keeps everything; 0 records nothing.
	SampleRate float64
}

// Enabled reports whether tracing should be wired up at all.
func (c Config) Enabled() bool { return c.Endpoint != "" }

// Provider owns the SDK tracer provider and the tracer the pipeline uses.
// A disabled Provider's methods are all safe: spans are no-ops and
// Shutdown is a no-op, so call sites never branch on whether tracing is on.
type Provider struct {
	tp      *sdktrace.TracerProvider
	Tracer  trace.Tracer
	enabled bool
}

// Enabled reports whether spans are actually being exported. False does not
// mean call sites must skip spans — the no-op tracer still threads context
// — only that nothing reaches a collector.
func (p *Provider) Enabled() bool { return p != nil && p.enabled }

// Shutdown flushes pending spans to the OTLP endpoint and releases the
// exporter. Safe to call on a disabled (or nil) provider; the shutdown path
// does exactly that so callers never need a nil/disabled check. The caller
// bounds the flush with its own context deadline so a hung collector delays
// shutdown by at most that long rather than hanging the process.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// New wraps an existing tracer into a Provider, sharing one construction
// path for call sites that already hold a trace.Tracer (tests inject an
// in-memory exporter's tracer this way instead of a real OTLP endpoint).
func New(tr trace.Tracer) *Provider {
	return &Provider{Tracer: tr}
}

// Setup wires the tracer provider. With an empty endpoint it installs
// nothing: the global provider stays the API's no-op default, so every
// span from context is a no-op span and the process overhead is a few
// interface calls per stage. Errors here fail startup — an operator who
// asked for tracing wants a bad exporter config refused at boot, not
// silently dropped.
func Setup(ctx context.Context, cfg Config) (*Provider, error) {
	if !cfg.Enabled() {
		return &Provider{Tracer: noopTracer()}, nil
	}

	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build OTLP exporter: %w", err)
	}

	name := cfg.ServiceName
	if name == "" {
		name = ServiceName
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", name)),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	// The parent-based sampler is the standard shape: a sampled root spans
	// the whole poll cycle, and every child (decode, rules, store,
	// delivery) follows the root's decision, so one trace is kept or
	// dropped as a unit. Sampling children independently would leave
	// half-traces, which are worse than none.
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	// Make this provider global so library code that starts spans off the
	// ambient global tracer (the SDK's own HTTP instrumentation, future
	// contributors) lands on the same trace.
	otel.SetTracerProvider(tp)

	return &Provider{
		tp:      tp,
		Tracer:  tp.Tracer("github.com/sorotrail/sorobeacon"),
		enabled: true,
	}, nil
}

func noopTracer() trace.Tracer {
	return noop.NewTracerProvider().Tracer("github.com/sorotrail/sorobeacon")
}

// TraceID returns a random 32-hex-char id, for operators (and tests) that
// want to correlate a poll cycle against a collector view before any span
// exists. A broken entropy source degrades to a time-derived id rather
// than failing a poll over telemetry.
func TraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// StartSpan starts a span named name, parented by whatever ctx carries.
// It returns the span (a no-op when tracing is off — the interface stays
// identical either way) and a ctx carrying it, so the next stage picks the
// parent up from the context without anyone passing spans by hand.
func (p *Provider) StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return p.Tracer.Start(ctx, name, opts...)
}
