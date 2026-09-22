package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/reqid"
)

func TestWriteErrRequestIDMatchesHeader(t *testing.T) {
	h := reqid.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, r, http.StatusNotFound, "not found")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))

	headerID := rec.Header().Get(reqid.Header)
	if headerID == "" {
		t.Fatal("response missing X-Request-ID")
	}

	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	got, _ := env["request_id"].(string)
	if got != headerID {
		t.Fatalf("writeErr request_id %q != X-Request-ID %q", got, headerID)
	}
}

func TestWriteErrHonoursIncomingRequestID(t *testing.T) {
	h := reqid.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, r, http.StatusBadRequest, "invalid JSON")
	}))

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	req.Header.Set(reqid.Header, "caller-supplied-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get(reqid.Header) != "caller-supplied-123" {
		t.Fatalf("header = %q, want caller-supplied-123", rec.Header().Get(reqid.Header))
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if got, _ := env["request_id"].(string); got != "caller-supplied-123" {
		t.Fatalf("writeErr request_id %q, want caller-supplied-123", got)
	}
}
