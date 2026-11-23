package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Database span attribute keys.
const (
	// AttrDBOperation is the statement's leading verb (SELECT, INSERT, ...).
	AttrDBOperation = "db.operation.name"
	// AttrDBStatementID identifies the statement text: a short hash of the
	// SQL with whitespace normalised. The SQL is parameterised, so the
	// statement identifies *which* query ran without any parameter value —
	// and the values (tenant data, workspace ids, payloads) never reach a
	// span.
	AttrDBStatementID = "db.statement.id"
)

// QueryTracer is a pgx query tracer that records one span per statement.
// Install it on a pool's ConnConfig.Tracer. A nil Provider yields a tracer
// whose methods do nothing.
type QueryTracer struct{ p *Provider }

// NewQueryTracer returns a tracer for pgx statements.
func (p *Provider) NewQueryTracer() *QueryTracer { return &QueryTracer{p: p} }

type dbSpanKey struct{}

// TraceQueryStart implements pgx.QueryTracer.
func (t *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t == nil || t.p == nil || t.p.Tracer == nil {
		return ctx
	}
	op, id := StatementIdentity(data.SQL)
	ctx, span := t.p.Tracer.Start(ctx, "db."+strings.ToLower(op),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String(AttrDBOperation, op),
			attribute.String(AttrDBStatementID, id),
		),
	)
	return context.WithValue(ctx, dbSpanKey{}, span)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (t *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(dbSpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil && data.Err != pgx.ErrNoRows {
		// The error class only: a driver error message can echo a value.
		span.SetStatus(codes.Error, "query failed")
	}
	span.End()
}

// StatementIdentity returns a statement's leading verb and a short stable
// hash of its normalised text. Span names use only the verb ("db.select"),
// a bounded set; the hash tells statements apart without the SQL or its
// parameters entering a span.
func StatementIdentity(sql string) (op, id string) {
	fields := strings.Fields(sql)
	op = "QUERY"
	if len(fields) > 0 {
		switch upper := strings.ToUpper(fields[0]); upper {
		case "SELECT", "INSERT", "UPDATE", "DELETE", "BEGIN", "COMMIT", "ROLLBACK", "CREATE", "ALTER", "DROP", "COPY", "LOCK", "SET", "WITH":
			op = upper
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, " ")))
	return op, hex.EncodeToString(sum[:6])
}
