package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func doReq(t *testing.T, h http.Handler, method, path, remote string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func TestRateLimitDisabledIsNoop(t *testing.T) {
	h := RateLimitMiddleware(RateLimitConfig{})(okHandler())
	for i := 0; i < 20; i++ {
		res := doReq(t, h, http.MethodGet, "/monitors", "192.0.2.1:1", nil)
		if res.Code != http.StatusOK {
			t.Fatalf("disabled limiter rejected request %d: %d", i, res.Code)
		}
		if got := res.Header().Get("RateLimit-Limit"); got != "" {
			t.Fatalf("disabled limiter must not set RateLimit headers, got %q", got)
		}
	}
}

func TestRateLimitAllowsThenDenies(t *testing.T) {
	h := RateLimitMiddleware(RateLimitConfig{RPS: 1, Burst: 2})(okHandler())
	remote := "192.0.2.10:9"

	for i := 0; i < 2; i++ {
		res := doReq(t, h, http.MethodGet, "/monitors", remote, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("allow %d = %d", i, res.Code)
		}
		if got := res.Header().Get("RateLimit-Limit"); got != "2" {
			t.Fatalf("RateLimit-Limit = %q, want 2", got)
		}
		if res.Header().Get("RateLimit-Remaining") == "" {
			t.Fatal("missing RateLimit-Remaining on allow")
		}
	}

	res := doReq(t, h, http.MethodGet, "/monitors", remote, nil)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("deny = %d, want 429", res.Code)
	}
	if got := res.Header().Get("Retry-After"); got == "" {
		t.Fatal("missing Retry-After")
	}
	if got := res.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Fatalf("RateLimit-Remaining = %q, want 0", got)
	}
	if got := res.Header().Get("RateLimit-Reset"); got == "" {
		t.Fatal("missing RateLimit-Reset")
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "rate limit exceeded" {
		t.Fatalf("error envelope = %#v", body)
	}
	if body["code"] != http.StatusText(http.StatusTooManyRequests) {
		t.Fatalf("code = %#v", body["code"])
	}
}

func TestRateLimitExemptsProbes(t *testing.T) {
	h := RateLimitMiddleware(RateLimitConfig{RPS: 1, Burst: 1})(okHandler())
	remote := "192.0.2.20:9"
	for _, path := range []string{"/health", "/livez", "/readyz", "/api/v1/health"} {
		for i := 0; i < 5; i++ {
			res := doReq(t, h, http.MethodGet, path, remote, nil)
			if res.Code != http.StatusOK {
				t.Fatalf("%s #%d = %d, probes must not be limited", path, i, res.Code)
			}
			if got := res.Header().Get("RateLimit-Limit"); got != "" {
				t.Fatalf("probe %s got RateLimit headers %q", path, got)
			}
		}
	}
}

func TestRateLimitDoesNotTrustForwardedByDefault(t *testing.T) {
	h := RateLimitMiddleware(RateLimitConfig{RPS: 1, Burst: 1})(okHandler())
	remote := "192.0.2.30:9"
	first := doReq(t, h, http.MethodGet, "/monitors", remote, http.Header{"X-Forwarded-For": []string{"198.51.100.1"}})
	if first.Code != http.StatusOK {
		t.Fatalf("first = %d", first.Code)
	}
	second := doReq(t, h, http.MethodGet, "/monitors", remote, http.Header{"X-Forwarded-For": []string{"198.51.100.2"}})
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For must share the RemoteAddr bucket, got %d", second.Code)
	}
}

func TestRateLimitTrustsForwardedWhenEnabled(t *testing.T) {
	h := RateLimitMiddleware(RateLimitConfig{RPS: 1, Burst: 1, TrustForwarded: true})(okHandler())
	remote := "192.0.2.40:9"
	a := doReq(t, h, http.MethodGet, "/monitors", remote, http.Header{"X-Forwarded-For": []string{"198.51.100.1, 203.0.113.1"}})
	b := doReq(t, h, http.MethodGet, "/monitors", remote, http.Header{"X-Forwarded-For": []string{"198.51.100.2"}})
	if a.Code != http.StatusOK || b.Code != http.StatusOK {
		t.Fatalf("distinct forwarded clients should not share a bucket: %d %d", a.Code, b.Code)
	}
}

func TestRateLimitEvictsIdle(t *testing.T) {
	cfg := RateLimitConfig{RPS: 1, Burst: 1, IdleTTL: time.Second}
	lim := newRateLimiter(cfg)
	now := time.Unix(1_700_000_000, 0)
	lim.now = func() time.Time { return now }

	h := lim.Middleware(okHandler())
	doReq(t, h, http.MethodGet, "/monitors", "192.0.2.50:1", nil)
	if n := lim.len(); n != 1 {
		t.Fatalf("len after first client = %d", n)
	}

	now = now.Add(2 * time.Second)
	doReq(t, h, http.MethodGet, "/monitors", "192.0.2.51:1", nil)
	if n := lim.len(); n != 1 {
		t.Fatalf("idle client should have been evicted, len = %d", n)
	}
}

func TestRoutesAppliesRateLimitAndExemptsHealth(t *testing.T) {
	s := New(&fakeStore{}, nil, nil, &fakeRPC{}, discardLogger()).WithRateLimit(RateLimitConfig{RPS: 1, Burst: 1})
	h := s.Routes()

	ok := doReq(t, h, http.MethodGet, "/version", "192.0.2.60:1", nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("version = %d", ok.Code)
	}
	denied := doReq(t, h, http.MethodGet, "/version", "192.0.2.60:1", nil)
	if denied.Code != http.StatusTooManyRequests {
		t.Fatalf("second version = %d, want 429", denied.Code)
	}
	for i := 0; i < 3; i++ {
		res := doReq(t, h, http.MethodGet, "/livez", "192.0.2.60:1", nil)
		if res.Code != http.StatusOK {
			t.Fatalf("livez still has to pass under a tight limit, got %d", res.Code)
		}
	}
}
