package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSAllowedOrigin(t *testing.T) {
	h := CORSMiddleware(CORSConfig{Origins: []string{"https://explorer.example"}})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Origin", "https://explorer.example")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "https://explorer.example" {
		t.Fatalf("allow-origin = %q", got)
	}
	if got := res.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("vary = %q, want Origin", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := CORSMiddleware(CORSConfig{Origins: []string{"*"}})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodOptions, "/api/events", nil)
	req.Header.Set("Origin", "https://any.example")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", res.Code)
	}
	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "https://any.example" {
		t.Fatalf("allow-origin = %q", got)
	}
}

func TestCORSPreflightCoverage(t *testing.T) {
	// OPTIONS must never reach the wrapped handler: a hostile page's
	// preflight is answered by the middleware alone. Disallowed and
	// empty-list cases must also omit CORS headers so the browser
	// blocks the response.
	const origin = "https://explorer.example"
	cases := []struct {
		name    string
		cfg     CORSConfig
		origin  string
		wantACA bool
	}{
		{
			name:    "allowed origin",
			cfg:     CORSConfig{Origins: []string{origin}},
			origin:  origin,
			wantACA: true,
		},
		{
			name:    "disallowed origin",
			cfg:     CORSConfig{Origins: []string{origin}},
			origin:  "https://evil.example",
			wantACA: false,
		},
		{
			name:    "empty allow-list disables CORS",
			cfg:     CORSConfig{},
			origin:  origin,
			wantACA: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handlerReached := false
			h := CORSMiddleware(tc.cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerReached = true
				w.WriteHeader(http.StatusTeapot)
			}))

			req := httptest.NewRequest(http.MethodOptions, "/api/events", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", "POST")
			req.Header.Set("Access-Control-Request-Headers", "Content-Type")
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)

			if handlerReached {
				t.Fatal("preflight reached the wrapped handler")
			}
			if res.Code != http.StatusNoContent {
				t.Fatalf("preflight = %d, want 204", res.Code)
			}

			allowOrigin := res.Header().Get("Access-Control-Allow-Origin")
			allowMethods := res.Header().Get("Access-Control-Allow-Methods")
			allowHeaders := res.Header().Get("Access-Control-Allow-Headers")
			maxAge := res.Header().Get("Access-Control-Max-Age")
			vary := res.Header().Get("Vary")

			if tc.wantACA {
				if allowOrigin != tc.origin {
					t.Fatalf("allow-origin = %q, want %q", allowOrigin, tc.origin)
				}
				if vary != "Origin" {
					t.Fatalf("vary = %q, want Origin (caches must key on Origin)", vary)
				}
				if !strings.Contains(allowMethods, "GET") || !strings.Contains(allowMethods, "POST") || !strings.Contains(allowMethods, "OPTIONS") {
					t.Fatalf("allow-methods = %q", allowMethods)
				}
				if !strings.Contains(allowHeaders, "Content-Type") {
					t.Fatalf("allow-headers = %q", allowHeaders)
				}
				if maxAge == "" {
					t.Fatal("missing Access-Control-Max-Age")
				}
				return
			}
			if allowOrigin != "" || allowMethods != "" || allowHeaders != "" || maxAge != "" {
				t.Fatalf("disallowed/disabled preflight leaked CORS headers: origin=%q methods=%q headers=%q max-age=%q",
					allowOrigin, allowMethods, allowHeaders, maxAge)
			}
		})
	}
}

func TestCORSDisallowedOriginGetsNoHeaders(t *testing.T) {
	h := CORSMiddleware(CORSConfig{Origins: []string{"https://explorer.example"}})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Origin", "https://evil.example")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin got CORS headers: %q", got)
	}
	if res.Code != http.StatusOK {
		t.Fatalf("non-preflight request must still be served, got %d", res.Code)
	}
}

func TestCORSEmptyConfigIsNoop(t *testing.T) {
	h := CORSMiddleware(CORSConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Origin", "https://any.example")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("empty config should emit no headers, got %q", got)
	}
}

func TestCORSAllowsWriteMethods(t *testing.T) {
	// SoroBeacon's API is read-write; a browser client must be able to
	// preflight POST/PATCH/DELETE, not just GET.
	h := CORSMiddleware(CORSConfig{Origins: []string{"https://ops.example"}})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/monitors", nil)
	req.Header.Set("Origin", "https://ops.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", res.Code)
	}
	if methods := res.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(methods, "POST") || !strings.Contains(methods, "DELETE") {
		t.Fatalf("allow-methods = %q, want POST and DELETE", methods)
	}
}
