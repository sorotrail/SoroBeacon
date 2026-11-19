package telemetry

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/propagation"
)

func httpHeader(r *http.Request) propagation.HeaderCarrier { return propagation.HeaderCarrier(r.Header) }

func pgxStart(sql string, args ...any) pgx.TraceQueryStartData {
	return pgx.TraceQueryStartData{SQL: sql, Args: args}
}

func pgxEnd(err error) pgx.TraceQueryEndData { return pgx.TraceQueryEndData{Err: err} }
