// Package reqid gives every HTTP request a short correlation ID.
//
// An incoming X-Request-ID is honoured (so a caller's own correlation chain
// survives); otherwise one is generated. The ID is echoed back on the
// response as X-Request-ID and stored in the request context for handlers
// and log lines. The error envelope carries it too, so a user quoting an
// error maps directly to one request in the logs.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// Header is the conventional correlation header, honoured both ways.
const Header = "X-Request-ID"

type ctxKey struct{}

// New returns a 16-hex-char random ID. crypto/rand, not math/rand: request
// IDs end up in logs and error responses, where predictability would let
// an outsider forge a plausible-looking correlation trail.
func New() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing means the system entropy source is broken;
		// an empty ID degrades correlation but never breaks a request.
		return ""
	}
	return hex.EncodeToString(b[:])
}

// From returns the request's ID: the one set by the middleware, or a fresh
// one when the request did not pass through it.
func From(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKey{}).(string); ok && v != "" {
		return v
	}
	return New()
}

// FromContext returns the request ID carried by ctx, or "" when the context
// never went through the middleware. Unlike From it does not generate one:
// background stages (the poller's loop) have no HTTP request behind them and
// must not invent a new id per stage — telemetry uses this to stamp spans
// with the correlation id, falling back to one id for the whole loop.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// WithContext returns ctx carrying id, the writer side of FromContext.
// The middleware uses it; tests and background stages that already hold a
// correlation id use it to keep a whole trace under one id.
func WithContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// Middleware assigns the request's ID, echoes it on the response, and
// stores it in the context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(Header)
		if id == "" || len(id) > 128 {
			id = New()
		}
		w.Header().Set(Header, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}
