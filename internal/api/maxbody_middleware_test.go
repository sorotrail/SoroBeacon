package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMaxBodyMiddleware exercises the middleware directly, without the full
// router, so its method filtering, Content-Length precheck and default limit
// are covered in isolation.
func TestMaxBodyMiddleware(t *testing.T) {
	tests := []struct {
		name        string
		limit       int64
		method      string
		body        string
		wantStatus  int
		wantReached bool
	}{
		{name: "POST under limit passes through", limit: 16, method: http.MethodPost, body: "hello", wantStatus: http.StatusOK, wantReached: true},
		{name: "PUT exactly at limit passes through", limit: 5, method: http.MethodPut, body: "hello", wantStatus: http.StatusOK, wantReached: true},
		{name: "PATCH over limit rejected with 413 before handler", limit: 4, method: http.MethodPatch, body: "hello", wantStatus: http.StatusRequestEntityTooLarge},
		{name: "POST over limit rejected with 413 before handler", limit: 4, method: http.MethodPost, body: "hello", wantStatus: http.StatusRequestEntityTooLarge},
		{name: "GET is not limited", limit: 1, method: http.MethodGet, body: "hello", wantStatus: http.StatusOK, wantReached: true},
		{name: "non-positive limit falls back to default", limit: 0, method: http.MethodPost, body: "hello", wantStatus: http.StatusOK, wantReached: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				got, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				if string(got) != tt.body {
					t.Fatalf("body = %q, want %q", got, tt.body)
				}
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(tt.method, "/x", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			MaxBodyMiddleware(tt.limit)(next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if reached != tt.wantReached {
				t.Fatalf("handler reached = %v, want %v", reached, tt.wantReached)
			}
			if tt.wantStatus == http.StatusRequestEntityTooLarge {
				env := decodeEnvelope(t, rec.Body)
				if env["error"] != "request body too large" {
					t.Fatalf("error = %v, want request body too large", env["error"])
				}
			}
		})
	}
}

// TestMaxBodyMiddleware_UnknownLengthTruncatedOnRead covers a body whose size
// is not declared up front (chunked): the precheck cannot see it, so the
// MaxBytesReader must stop the read once the limit is crossed.
func TestMaxBodyMiddleware_UnknownLengthTruncatedOnRead(t *testing.T) {
	var readErr error
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", 64)))
	req.ContentLength = -1
	MaxBodyMiddleware(8)(next).ServeHTTP(httptest.NewRecorder(), req)

	var maxErr *http.MaxBytesError
	if !errors.As(readErr, &maxErr) {
		t.Fatalf("read error = %v, want *http.MaxBytesError", readErr)
	}
	if maxErr.Limit != 8 {
		t.Fatalf("MaxBytesError.Limit = %d, want 8", maxErr.Limit)
	}
}
