package reqid

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareAssignsAndEchoesID(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = From(r)
		w.WriteHeader(http.StatusOK)
	}))

	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("handler saw no request ID in context")
	}
	if got := res.Header().Get(Header); got != seen {
		t.Fatalf("response header %q != context ID %q", got, seen)
	}
}

func TestMiddlewareHonoursIncomingID(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = From(r)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(Header, "caller-supplied-123")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	if seen != "caller-supplied-123" {
		t.Fatalf("incoming ID not honoured: got %q", seen)
	}
	if res.Header().Get(Header) != "caller-supplied-123" {
		t.Fatal("incoming ID not echoed on response")
	}
}

func TestMiddlewareReplacesOversizedID(t *testing.T) {
	// An unbounded honouring of incoming IDs would let a caller pin
	// arbitrarily large values into logs and responses.
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(Header, string(long))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	if got := res.Header().Get(Header); len(got) != 16 {
		t.Fatalf("oversized ID should be replaced with a 16-char ID, got %d chars", len(got))
	}
}

func TestNewUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := New()
		if len(id) != 16 {
			t.Fatalf("ID %q: want 16 hex chars", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %q within 1000 draws", id)
		}
		seen[id] = true
	}
}
