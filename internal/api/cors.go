package api

import (
	"net/http"
	"strings"
)

// CORSConfig controls the CORS middleware. Origins is the allow-list of
// Origins permitted to call the API from a browser; an empty list disables
// CORS entirely — same-origin pages (SoroBeacon's own dashboard) never
// need it. Think hard before allowing a third-party origin: the API is
// unauthenticated and mutates monitors and channels.
type CORSConfig struct {
	Origins []string
}

// Allowed reports whether origin may cross-call the API. An origin of "*"
// in the allow-list permits any origin, which is reasonable for a public,
// read-only, unauthenticated API whose data is chain-public anyway.
func (c CORSConfig) Allowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, o := range c.Origins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// CORSMiddleware adds CORS response headers for allowed origins and
// answers preflight OPTIONS requests. Disallowed origins get no CORS
// headers: the browser blocks the response, which is the correct outcome.
func CORSMiddleware(cfg CORSConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if cfg.Allowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, X-Request-ID")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			if r.Method == http.MethodOptions && origin != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
