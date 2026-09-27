package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorotrail/sorobeacon/internal/reqid"
)

// Span attribute keys. lower_snake_case, matching the codebase's slog
// convention, so an operator grepping a log line can grep a span the same
// way. Only non-secret identifiers belong here: an attribute is a filter
// label in every tracing backend, and labels are for filtering, never for
// payloads — see SetAttrs for the hard rule.
const (
	// AttrRequestID carries the X-Request-ID so a log line and its trace
	// can be joined. This is the logs-to-traces bridge the issue asks for.
	AttrRequestID = "request_id"
	// AttrRuleID and AttrRuleType identify which configured objects a stage
	// worked on (AttrMonitorID, AttrAlertID likewise).
	AttrMonitorID = "monitor_id"
	AttrRuleID    = "rule_id"
	AttrRuleType  = "rule_type"
	AttrAlertID   = "alert_id"
	// AttrChannelID identifies the delivery target by row id. Never the
	// channel's name or config: those are operator-chosen strings, and the
	// config holds the secrets. AttrChannelType is the static notifier kind
	// ("discord", "slack", ...), the same bounded set /metrics uses.
	AttrChannelID   = "channel_id"
	AttrChannelType = "channel_type"
	// AttrAttempts counts delivery attempts on the channel's span.
	AttrAttempts = "attempts"
	// AttrEventID is the source event's TOID id; AttrContractID the
	// contract being watched. Both are public chain data.
	AttrEventID    = "event_id"
	AttrContractID = "contract_id"
	// AttrOutcome records what a store write did (created | duplicate |
	// suppressed).
	AttrOutcome = "outcome"
	// AttrStartLedger and AttrContractsWatched describe a fetch page's
	// request shape; AttrEventsScanned / AttrEventsMatched summarise the
	// cycle on the root span.
	AttrStartLedger      = "start_ledger"
	AttrContractsWatched = "contracts_watched"
	AttrEventsScanned    = "events_scanned"
	AttrEventsMatched    = "events_matched"
)

// WithRequestID starts a span carrying the ambient request id (reqid) as an
// attribute, so a trace can be joined with its log lines. The id is
// generated on the spot when the context never went through the middleware
// (the poller's loop has no HTTP request behind it), which keeps the
// attribute present and consistent for the whole trace.
func (p *Provider) WithRequestID(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	id := reqid.FromContext(ctx)
	if id == "" {
		id = reqid.New()
	}
	opts = append(opts, trace.WithAttributes(attribute.String(AttrRequestID, id)))
	return p.StartSpan(ctx, name, opts...)
}

// SpanID returns the span's trace-scoped id as hex, or "" when the span is
// a no-op. Handlers that log the id can then correlate their structured
// lines with the exported trace.
func SpanID(span trace.Span) string {
	sc := span.SpanContext()
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SetAttrs sets only string, int64 and bool attributes. Deliberately no
// slice or map form: a map makes it trivial to dump a channel config into a
// span, and a span attribute is a label in every backend — labels are for
// filtering, never for payloads. Channel names, configs, webhook URLs, bot
// tokens and SMTP credentials must never be passed here; identify channels
// and rules by row id instead.
func SetAttrs(span trace.Span, kvs ...any) {
	if span == nil {
		return
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		k, ok := kvs[i].(string)
		if !ok {
			continue
		}
		switch v := kvs[i+1].(type) {
		case string:
			span.SetAttributes(attribute.String(k, v))
		case int64:
			span.SetAttributes(attribute.Int64(k, v))
		case int:
			span.SetAttributes(attribute.Int(k, v))
		case bool:
			span.SetAttributes(attribute.Bool(k, v))
		default:
			// Unsupported types are skipped rather than stringified: a
			// struct or slice forced into a string is exactly how secrets
			// end up in a label by accident.
			continue
		}
	}
}

// RecordError marks a span as failed with the error attached. The error
// message ends up in the span: constructors and notifiers keep secrets out
// of error text (see CONTRIBUTING), and this helper relies on it, so only
// pass errors that are already safe to log.
func RecordError(span trace.Span, err error) {
	// A nil span means the call site runs with tracing compiled out of its
	// wiring (a nil Provider); dropping the record beats a nil panic there.
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// NoopSpan returns a do-nothing span for call sites that conditionally
// start real spans: returning this instead of nil keeps the error-handling
// and attribute calls after it uniform. It carries no trace context, so
// code paths that never start a real span stay completely off the wire.
func NoopSpan() trace.Span {
	return trace.SpanFromContext(context.Background())
}
