package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
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

func TestRuleBuilderFields_StartsWithSchemaDefault(t *testing.T) {
	body := builderFields([]rules.FieldSchema{
		{Name: "flag", Type: "boolean", Default: "true"},
		{Name: "outside", Type: "boolean", Default: "false"},
		{Name: "threshold", Type: "number", Default: "10"},
	}, nil)
	if !strings.Contains(body, `<option value="true" selected>true</option>`) {
		t.Errorf("boolean field did not select its default: %s", body)
	}
	if !strings.Contains(body, `<option value="false" selected>false</option>`) {
		t.Errorf("boolean field did not select its default: %s", body)
	}
	if !strings.Contains(body, `value="10"`) {
		t.Errorf("number field did not render its default: %s", body)
	}

	body = builderFields([]rules.FieldSchema{{Name: "flag", Type: "boolean", Default: "true"}},
		map[string]string{"flag": "false"})
	if !strings.Contains(body, `<option value="false" selected>false</option>`) {
		t.Errorf("submitted value did not override the default: %s", body)
	}
}

// TestRuleBuilderCreatesBooleanOptionAsJSONBoolean is the end-to-end guard for
// the bug: exclude=true used to be stored as the string "true" and rejected by
// the evaluator after the form had already been accepted.
func TestRuleBuilderCreatesBooleanOptionAsJSONBoolean(t *testing.T) {
	const contractID = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
	st := &ruleBuilderStore{monitor: store.Monitor{ID: 1, Name: "treasury"}}
	srv := newRuleBuilderServer(t, st)
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.PostForm(srv.URL+"/monitors/1/rules", url.Values{
		"type":               {"contract_allowlist"},
		"use_builder":        {"true"},
		"param_contract_ids": {`["` + contractID + `"]`},
		"param_exclude":      {"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", res.StatusCode, res.Status)
	}
	if len(st.created) != 1 {
		t.Fatalf("created %d rules, want 1", len(st.created))
	}
	assert.JSONEq(t, `{"contract_ids":["`+contractID+`"],"exclude":true}`, string(st.created[0].Params))
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
	raw, err := rules.ParamsFromForm(schema, values)
	if err != nil {
		t.Fatalf("ParamsFromForm: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"event_name":"transfer"`) {
		t.Errorf("expected event_name in params, got: %s", s)
	}
	if !strings.Contains(s, `"count":50`) {
		t.Errorf("expected count as number in params, got: %s", s)
	}
}

// ruleBuilderStore answers the monitor page and records the rules the builder
// created, so a test can assert that a rejected form stored nothing.
type ruleBuilderStore struct {
	emptyStore
	monitor store.Monitor
	created []store.Rule
}

func (s *ruleBuilderStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	m := s.monitor
	return &m, nil
}

func (s *ruleBuilderStore) ListRules(context.Context, int64, bool) ([]store.Rule, error) {
	return nil, nil
}

func (s *ruleBuilderStore) CreateRule(_ context.Context, r *store.Rule) error {
	s.created = append(s.created, *r)
	return nil
}

func newRuleBuilderServer(t *testing.T, st *ruleBuilderStore) *httptest.Server {
	t.Helper()
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return httptest.NewServer(s.Routes())
}

// TestRuleBuilderRejectsNonNumericNumberField: a number field that does not
// hold a number is reported by name. It used to be dropped, which took every
// other field of the form with it, and the dashboard then complained about a
// field the user had never touched.
func TestRuleBuilderRejectsNonNumericNumberField(t *testing.T) {
	st := &ruleBuilderStore{monitor: store.Monitor{ID: 1, Name: "treasury"}}
	srv := newRuleBuilderServer(t, st)
	defer srv.Close()

	res, err := http.PostForm(srv.URL+"/monitors/1/rules", url.Values{
		"type":         {"frequency_threshold"},
		"use_builder":  {"true"},
		"param_count":  {"many"},
		"param_window": {"5m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "count must be a number") {
		t.Errorf("error does not name the malformed field: %s", body)
	}
	if len(st.created) != 0 {
		t.Errorf("a rejected form created %d rules, want none", len(st.created))
	}
	// The user's own input comes back with the form.
	if !strings.Contains(string(body), `value="many"`) {
		t.Errorf("the form did not keep the value the user typed: %s", body)
	}
	if !strings.Contains(string(body), `value="5m"`) {
		t.Errorf("the form did not keep the other value the user typed: %s", body)
	}
}

// TestRuleBuilderRejectsInvalidJSONObjectField: an object field that is not
// valid JSON is reported as such, not as a missing field.
func TestRuleBuilderRejectsInvalidJSONObjectField(t *testing.T) {
	st := &ruleBuilderStore{monitor: store.Monitor{ID: 1, Name: "treasury"}}
	srv := newRuleBuilderServer(t, st)
	defer srv.Close()

	res, err := http.PostForm(srv.URL+"/monitors/1/rules", url.Values{
		"type":           {"event_name_glob"},
		"use_builder":    {"true"},
		"param_patterns": {`["swap_*"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "patterns is not valid JSON") {
		t.Errorf("error does not name the malformed field: %s", body)
	}
	if strings.Contains(string(body), "patterns is required") {
		t.Errorf("error blames a field the user did fill in: %s", body)
	}
	if len(st.created) != 0 {
		t.Errorf("a rejected form created %d rules, want none", len(st.created))
	}
	if !strings.Contains(string(body), `["swap_*"`) {
		t.Errorf("the form did not keep the value the user typed: %s", body)
	}
}

// TestRuleBuilderCreatesRuleFromValidForm: the happy path still works, and
// the params carry the typed values.
func TestRuleBuilderCreatesRuleFromValidForm(t *testing.T) {
	st := &ruleBuilderStore{monitor: store.Monitor{ID: 1, Name: "treasury"}}
	srv := newRuleBuilderServer(t, st)
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.PostForm(srv.URL+"/monitors/1/rules", url.Values{
		"type":         {"frequency_threshold"},
		"use_builder":  {"true"},
		"param_count":  {"50"},
		"param_window": {"5m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", res.StatusCode, res.Status)
	}
	if len(st.created) != 1 {
		t.Fatalf("created %d rules, want 1", len(st.created))
	}
	assert.JSONEq(t, `{"count":50,"window":"5m"}`, string(st.created[0].Params))
}
