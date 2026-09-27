package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// inhibitionFakeStore is an in-memory inhibition slice plus two canned
// rules, so the handler tests cover validation, existence checks and the
// delete path without a database.
type inhibitionFakeStore struct {
	fakeStore
	rules map[int64]store.Rule
	pairs map[[2]int64]store.Inhibition
}

func newInhibitionFakeStore() *inhibitionFakeStore {
	return &inhibitionFakeStore{
		rules: map[int64]store.Rule{
			7: {ID: 7, MonitorID: 1, Type: "event_emitted"},
			9: {ID: 9, MonitorID: 1, Type: "event_emitted"},
		},
		pairs: map[[2]int64]store.Inhibition{},
	}
}

func (f *inhibitionFakeStore) GetRule(_ context.Context, id int64) (*store.Rule, error) {
	r, ok := f.rules[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &r, nil
}

func (f *inhibitionFakeStore) CreateInhibition(_ context.Context, in *store.Inhibition) error {
	if in.FiringWindowSeconds <= 0 {
		in.FiringWindowSeconds = store.DefaultInhibitionWindowSeconds
	}
	f.pairs[[2]int64{in.SourceRuleID, in.TargetRuleID}] = *in
	return nil
}

func (f *inhibitionFakeStore) ListInhibitions(context.Context) ([]store.Inhibition, error) {
	out := []store.Inhibition{}
	for _, in := range f.pairs {
		out = append(out, in)
	}
	return out, nil
}

func (f *inhibitionFakeStore) ListInhibitionsForTarget(_ context.Context, target int64) ([]store.Inhibition, error) {
	var out []store.Inhibition
	for _, in := range f.pairs {
		if in.TargetRuleID == target {
			out = append(out, in)
		}
	}
	return out, nil
}

func (f *inhibitionFakeStore) DeleteInhibition(_ context.Context, source, target int64) error {
	if _, ok := f.pairs[[2]int64{source, target}]; !ok {
		return store.ErrNotFound
	}
	delete(f.pairs, [2]int64{source, target})
	return nil
}

func newInhibitionServer(st *inhibitionFakeStore) http.Handler {
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	return s.Routes()
}

func doInhibitionRequest(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateInhibition(t *testing.T) {
	h := newInhibitionServer(newInhibitionFakeStore())
	rec := doInhibitionRequest(t, h, http.MethodPost, "/inhibitions",
		map[string]any{"source_rule_id": 7, "target_rule_id": 9})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var got store.Inhibition
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SourceRuleID != 7 || got.TargetRuleID != 9 {
		t.Errorf("pair = %+v, want 7->9", got)
	}
	if got.FiringWindowSeconds != store.DefaultInhibitionWindowSeconds {
		t.Errorf("window = %d, want default %d", got.FiringWindowSeconds, store.DefaultInhibitionWindowSeconds)
	}
}

func TestCreateInhibitionValidation(t *testing.T) {
	h := newInhibitionServer(newInhibitionFakeStore())
	for name, body := range map[string]any{
		"missing ids":   map[string]any{},
		"self inhibit":  map[string]any{"source_rule_id": 7, "target_rule_id": 7},
		"negative win":  map[string]any{"source_rule_id": 7, "target_rule_id": 9, "firing_window_seconds": -5},
		"zero source":   map[string]any{"source_rule_id": 0, "target_rule_id": 9},
	} {
		rec := doInhibitionRequest(t, h, http.MethodPost, "/inhibitions", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateInhibitionUnknownRule(t *testing.T) {
	h := newInhibitionServer(newInhibitionFakeStore())
	rec := doInhibitionRequest(t, h, http.MethodPost, "/inhibitions",
		map[string]any{"source_rule_id": 7, "target_rule_id": 4242})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestListAndDeleteInhibition(t *testing.T) {
	st := newInhibitionFakeStore()
	h := newInhibitionServer(st)

	rec := doInhibitionRequest(t, h, http.MethodGet, "/inhibitions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var empty struct {
		Inhibitions []store.Inhibition `json:"inhibitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(empty.Inhibitions) != 0 {
		t.Fatalf("list = %v, want empty", empty.Inhibitions)
	}

	rec = doInhibitionRequest(t, h, http.MethodPost, "/inhibitions",
		map[string]any{"source_rule_id": 7, "target_rule_id": 9, "firing_window_seconds": 60})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	rec = doInhibitionRequest(t, h, http.MethodGet, "/inhibitions", nil)
	var listed struct {
		Inhibitions []store.Inhibition `json:"inhibitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Inhibitions) != 1 || listed.Inhibitions[0].FiringWindowSeconds != 60 {
		t.Fatalf("list = %+v, want the 60s pair", listed.Inhibitions)
	}

	rec = doInhibitionRequest(t, h, http.MethodDelete, "/inhibitions/7/9", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	rec = doInhibitionRequest(t, h, http.MethodDelete, "/inhibitions/7/9", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", rec.Code)
	}
}
