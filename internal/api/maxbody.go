package api

import (
	"net/http"
)

// MaxBodyMiddleware wraps POST/PATCH/PUT bodies with http.MaxBytesReader
// so an oversized write is truncated during the read rather than buffered
// first. GET/HEAD/OPTIONS are left alone so probes and list endpoints stay
// unaffected. When Content-Length is already above the limit the request
// is rejected with 413 before any read, through the same JSON envelope as
// other API errors — not a connection reset.
func MaxBodyMiddleware(limit int64) func(http.Handler) http.Handler {
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPatch, http.MethodPut:
				if r.ContentLength > limit {
					writeErr(w, r, http.StatusRequestEntityTooLarge, "request body too large")
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}
