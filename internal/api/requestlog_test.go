package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func newLogBuf() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf, log
}

func parseLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected a log line")
	}
	// slog JSON handler writes one object per line; take the last in case
	// chi Recoverer also logged.
	lines := strings.Split(line, "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("decode log: %v raw=%q", err, line)
	}
	return rec
}

func TestRequestLogEmitsFieldsForHandledRequest(t *testing.T) {
	buf, log := newLogBuf()
	r := chi.NewRouter()
	r.Use(RequestLog(log))
	r.Get("/monitors/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/monitors/42?token=secret", nil)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	rec := parseLogLine(t, buf)
	if rec["msg"] != "http request" {
		t.Fatalf("msg = %#v", rec["msg"])
	}
	if rec["method"] != http.MethodGet {
		t.Fatalf("method = %#v", rec["method"])
	}
	if rec["route"] != "/monitors/{id}" {
		t.Fatalf("route = %#v, want matched pattern not raw path", rec["route"])
	}
	if rec["status"] != float64(http.StatusOK) {
		t.Fatalf("status = %#v", rec["status"])
	}
	if rec["bytes"] != float64(2) {
		t.Fatalf("bytes = %#v", rec["bytes"])
	}
	if rec["level"] != "DEBUG" {
		t.Fatalf("level = %#v, want DEBUG for 2xx", rec["level"])
	}
	if _, ok := rec["duration"]; !ok {
		t.Fatalf("missing duration: %#v", rec)
	}
	if _, ok := rec["request_id"]; !ok {
		t.Fatalf("missing request_id: %#v", rec)
	}
	raw := buf.String()
	if strings.Contains(raw, "token=secret") || strings.Contains(raw, "/monitors/42") {
		t.Fatalf("logged raw path or query: %s", raw)
	}
}

func TestRequestLogPanicRecoveredStillEmits(t *testing.T) {
	buf, log := newLogBuf()
	r := chi.NewRouter()
	r.Use(RequestLog(log), middleware.Recoverer)
	r.Get("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 from Recoverer", res.Code)
	}
	rec := parseLogLine(t, buf)
	if rec["route"] != "/boom" {
		t.Fatalf("route = %#v", rec["route"])
	}
	if rec["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("status = %#v, want 500", rec["status"])
	}
	if rec["level"] != "ERROR" {
		t.Fatalf("level = %#v, want ERROR for 5xx", rec["level"])
	}
}

func TestRequestLogProbesStayDebugOnFailure(t *testing.T) {
	buf, log := newLogBuf()
	r := chi.NewRouter()
	r.Use(RequestLog(log))
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	rec := parseLogLine(t, buf)
	if rec["level"] != "DEBUG" {
		t.Fatalf("level = %#v, want DEBUG for probes even on 5xx", rec["level"])
	}
	if rec["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("status = %#v", rec["status"])
	}
}

func TestRequestLog4xxIsInfo(t *testing.T) {
	buf, log := newLogBuf()
	r := chi.NewRouter()
	r.Use(RequestLog(log))
	r.Get("/monitors/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	})

	req := httptest.NewRequest(http.MethodGet, "/monitors/9", nil)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	rec := parseLogLine(t, buf)
	if rec["level"] != "INFO" {
		t.Fatalf("level = %#v, want INFO for 4xx", rec["level"])
	}
	if rec["route"] != "/monitors/{id}" {
		t.Fatalf("route = %#v", rec["route"])
	}
}
