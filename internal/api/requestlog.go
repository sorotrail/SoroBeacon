package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sorotrail/sorobeacon/internal/reqid"
)

// RequestLog emits one structured slog line per request: method, the
// matched chi route pattern (not the raw path — that would explode
// cardinality and leak query-shaped secrets), status, duration, bytes
// written, and request ID. Query strings, headers and bodies are never
// logged; they carry webhook URLs, tokens and SMTP credentials.
//
// 2xx/3xx are debug. 4xx is info. 5xx is error. Health and readiness
// probes stay debug even when they fail, so a Kubernetes scrape every
// second cannot drown the log.
func RequestLog(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []any{
				"method", r.Method,
				"route", routePattern(r),
				"status", status,
				"duration", time.Since(start),
				"bytes", ww.BytesWritten(),
				"request_id", reqid.From(r),
			}
			msg := "http request"
			switch {
			case isProbe(r):
				log.Debug(msg, attrs...)
			case status >= 500:
				log.Error(msg, attrs...)
			case status >= 400:
				log.Info(msg, attrs...)
			default:
				log.Debug(msg, attrs...)
			}
		})
	}
}

func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	// Unmatched requests have no pattern; log the path without the query.
	return r.URL.Path
}

func isProbe(r *http.Request) bool {
	p := routePattern(r)
	if p == "" {
		p = r.URL.Path
	}
	return strings.HasSuffix(p, "/livez") || strings.HasSuffix(p, "/readyz") || strings.HasSuffix(p, "/health")
}
