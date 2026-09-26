package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
)

func TestRuleBuilderFields_KnownType(t *testing.T) {
	reg := rules.NewRegistry()
	srv, err := New(emptyStore{}, reg, notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r := srv.Routes()
	req := httptest.NewRequest(http.MethodGet, "/rulebuilder/event_emitted", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "param_event_name") {
		t.Errorf("expected param_event_name field in builder output, got: %s", body)
	}
}

func TestRuleBuilderFields_UnknownType(t *testing.T) {
	reg := rules.NewRegistry()
	srv, err := New(emptyStore{}, reg, notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r := srv.Routes()
	req := httptest.NewRequest(http.MethodGet, "/rulebuilder/nonexistent", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("expected empty body for unknown rule type, got: %s", w.Body.String())
	}
}

func TestRuleBuilderFields_ValueThresholdHasSelect(t *testing.T) {
	reg := rules.NewRegistry()
	srv, err := New(emptyStore{}, reg, notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r := srv.Routes()
	req := httptest.NewRequest(http.MethodGet, "/rulebuilder/value_threshold", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `<select`) {
		t.Errorf("expected select element for comparison field, got: %s", body)
	}
	if !strings.Contains(body, "param_comparison") {
		t.Errorf("expected param_comparison in output")
	}
}

func TestParamsFromForm(t *testing.T) {
	schema := []rules.FieldSchema{
		{Name: "event_name", Type: "string"},
		{Name: "count", Type: "number"},
	}
	values := map[string]string{
		"event_name": "transfer",
		"count":      "50",
	}
	raw := rules.ParamsFromForm(schema, values)
	s := string(raw)
	if !strings.Contains(s, `"event_name":"transfer"`) {
		t.Errorf("expected event_name in params, got: %s", s)
	}
	if !strings.Contains(s, `"count":50`) {
		t.Errorf("expected count as number in params, got: %s", s)
	}
}
