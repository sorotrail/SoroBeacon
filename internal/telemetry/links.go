package telemetry

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// StartLinked starts a new root span linked to cause, for work that happens
// because of an earlier span but outside its lifetime: a delivery on another
// goroutine after detection, or an escalation step minutes later.
//
// A parent-child relationship across those boundaries would keep the
// detection trace open until the last escalation fired — traces that never
// end. A link records the causality without that, and every tracing backend
// can follow it. An invalid cause (no span in the detecting context, or one
// forgotten across a restart) starts a plain root span.
//
// The request id is carried over from ctx, as WithRequestID does.
func (p *Provider) StartLinked(ctx context.Context, name string, cause trace.SpanContext, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	opts = append(opts, trace.WithNewRoot())
	if cause.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{
			SpanContext: cause,
			Attributes:  []attribute.KeyValue{attribute.String(AttrLinkKind, "caused_by")},
		}))
	}
	return p.WithRequestID(ctx, name, opts...)
}

// AttrLinkKind labels why a link exists ("caused_by").
const AttrLinkKind = "link_kind"

// DefaultAlertLinkCapacity bounds how many alerts' detection spans are
// remembered for escalation links.
const DefaultAlertLinkCapacity = 10_000

// AlertLinks remembers the span each alert was detected in, so an
// escalation step delivered minutes later can link back to it.
//
// It is in memory and bounded: the oldest entries are forgotten first, and
// a restart forgets everything. An escalation whose detection span is gone
// still gets its own span, carrying the alert id, just without the link —
// tracing is never load-bearing. A nil *AlertLinks is safe to use.
type AlertLinks struct {
	mu    sync.Mutex
	max   int
	order []int64
	spans map[int64]trace.SpanContext
}

// NewAlertLinks returns a store holding at most capacity alerts.
func NewAlertLinks(capacity int) *AlertLinks {
	if capacity <= 0 {
		capacity = DefaultAlertLinkCapacity
	}
	return &AlertLinks{max: capacity, spans: make(map[int64]trace.SpanContext, capacity)}
}

// Remember records the span an alert was detected in. An invalid span
// context is ignored.
func (l *AlertLinks) Remember(alertID int64, sc trace.SpanContext) {
	if l == nil || !sc.IsValid() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.spans[alertID]; !ok {
		l.order = append(l.order, alertID)
	}
	l.spans[alertID] = sc
	for len(l.order) > l.max {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.spans, oldest)
	}
}

// Lookup returns the span an alert was detected in, when it is remembered.
func (l *AlertLinks) Lookup(alertID int64) (trace.SpanContext, bool) {
	if l == nil {
		return trace.SpanContext{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	sc, ok := l.spans[alertID]
	return sc, ok
}
