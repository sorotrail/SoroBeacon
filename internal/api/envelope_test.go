package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// documentedEnvelopeKeys is the writeErr JSON contract. A rename here is a
// client break; extra fields would mean the envelope grew silently.
var documentedEnvelopeKeys = []string{"error", "code", "request_id"}

// envelopeStore drives GetMonitor so handler tests can produce 404 (ErrNotFound)
// and 500 (any other error) without a real database.
type envelopeStore struct {
	store.Store
	err error
}

func (s *envelopeStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	return nil, s.err
}

func envelopeServer(st store.Store) *httptest.Server {
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	// Production mounts Routes() under /api/v1 behind reqid.Middleware;
	// wrapping here so the envelope's request_id matches the header.
	return httptest.NewServer(reqid.Middleware(s.Routes()))
}

func doEnvelope(t *testing.T, st store.Store, method, path, body, wantID string) (status int, ct string, raw map[string]any, rawBody []byte, hdrID string) {
	t.Helper()
	srv := envelopeServer(st)
	defer srv.Close()

	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if wantID != "" {
		req.Header.Set(reqid.Header, wantID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rawBody, err = io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw = map[string]any{}
	if len(rawBody) > 0 {
		if err := json.Unmarshal(rawBody, &raw); err != nil {
			t.Fatalf("decode: %v body=%s", err, rawBody)
		}
	}
	return res.StatusCode, res.Header.Get("Content-Type"), raw, rawBody, res.Header.Get(reqid.Header)
}

func assertEnvelope(t *testing.T, status int, ct string, raw map[string]any, body []byte, hdrID, wantMsg string, wantStatus int) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", status, wantStatus, body)
	}
	if ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	for _, key := range documentedEnvelopeKeys {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing field %q in %s", key, body)
		}
	}
	if len(raw) != len(documentedEnvelopeKeys) {
		t.Fatalf("unexpected extra fields: %v", raw)
	}
	if got, _ := raw["error"].(string); got != wantMsg {
		t.Fatalf("error = %q, want %q", got, wantMsg)
	}
	if got, _ := raw["code"].(string); got != http.StatusText(wantStatus) {
		t.Fatalf("code = %q, want %q", got, http.StatusText(wantStatus))
	}
	reqID, _ := raw["request_id"].(string)
	if reqID == "" {
		t.Fatalf("request_id empty in envelope: %s", body)
	}
	if hdrID == "" {
		t.Fatalf("X-Request-ID empty on response")
	}
	if reqID != hdrID {
		t.Fatalf("request_id %q != X-Request-ID %q", reqID, hdrID)
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	const wantID = "envelope-test-id"

	t.Run("400", func(t *testing.T) {
		status, ct, raw, body, hdrID := doEnvelope(t, &envelopeStore{}, http.MethodGet, "/monitors/abc", "", wantID)
		assertEnvelope(t, status, ct, raw, body, hdrID, "invalid id", http.StatusBadRequest)
		if hdrID != wantID {
			t.Fatalf("X-Request-ID = %q, want incoming %q", hdrID, wantID)
		}
	})

	t.Run("404", func(t *testing.T) {
		status, ct, raw, body, hdrID := doEnvelope(t, &envelopeStore{err: store.ErrNotFound}, http.MethodGet, "/monitors/99", "", wantID)
		assertEnvelope(t, status, ct, raw, body, hdrID, "not found", http.StatusNotFound)
		if hdrID != wantID {
			t.Fatalf("X-Request-ID = %q, want incoming %q", hdrID, wantID)
		}
	})

	t.Run("500", func(t *testing.T) {
		status, ct, raw, body, hdrID := doEnvelope(t, &envelopeStore{err: errors.New("db down")}, http.MethodGet, "/monitors/99", "", wantID)
		assertEnvelope(t, status, ct, raw, body, hdrID, "internal error", http.StatusInternalServerError)
		if hdrID != wantID {
			t.Fatalf("X-Request-ID = %q, want incoming %q", hdrID, wantID)
		}
	})
}

func TestErrorEnvelopeGeneratedRequestID(t *testing.T) {
	status, ct, raw, body, hdrID := doEnvelope(t, &envelopeStore{}, http.MethodGet, "/monitors/abc", "", "")
	assertEnvelope(t, status, ct, raw, body, hdrID, "invalid id", http.StatusBadRequest)
	if len(hdrID) != 16 {
		t.Fatalf("generated X-Request-ID %q: want 16 hex chars", hdrID)
	}
}
