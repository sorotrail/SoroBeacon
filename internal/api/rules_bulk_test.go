package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestCreateRulesBulk_Success(t *testing.T) {
	st := &validationStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal([]map[string]any{
		{"type": "event_emitted", "params": map[string]any{"event_name": "transfer"}},
		{"type": "token_event", "params": map[string]any{"event": "mint"}, "enabled": false},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules/bulk", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", res.StatusCode, raw)
	}
	if st.createdRules != 2 {
		t.Fatalf("created %d rules, want 2", st.createdRules)
	}
	var got []store.Rule
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rules, want 2", len(got))
	}
	if got[0].ID != 1 || got[0].Type != "event_emitted" || !got[0].Enabled {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].ID != 2 || got[1].Type != "token_event" || got[1].Enabled {
		t.Fatalf("second = %+v", got[1])
	}
	if got[0].MonitorID != 1 || got[1].MonitorID != 1 {
		t.Fatalf("monitor ids = %d, %d", got[0].MonitorID, got[1].MonitorID)
	}
}

func TestCreateRulesBulk_MidArrayValidationWritesNothing(t *testing.T) {
	st := &validationStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal([]map[string]any{
		{"type": "event_emitted", "params": map[string]any{"event_name": "transfer"}},
		{"type": "not-a-rule", "params": map[string]any{}},
		{"type": "event_emitted", "params": map[string]any{"event_name": "mint"}},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules/bulk", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", res.StatusCode, raw)
	}
	if st.createdRules != 0 {
		t.Fatalf("created %d rules after validation failure, want 0", st.createdRules)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Details) == 0 {
		t.Fatalf("expected details naming the failed index, got %s", raw)
	}
	if env.Details[0].Field != "rules[1].type" {
		t.Fatalf("field = %q, want rules[1].type; body %s", env.Details[0].Field, raw)
	}
}

func TestCreateRulesBulk_EmptyArray(t *testing.T) {
	res, env := postJSON(t, "/monitors/1/rules/bulk", []any{})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(env.Details) == 0 || env.Details[0].Field != "rules" {
		t.Fatalf("details = %+v, want field rules", env.Details)
	}
}

func TestCreateRulesBulk_Oversize(t *testing.T) {
	batch := make([]map[string]any, maxBulkRules+1)
	for i := range batch {
		batch[i] = map[string]any{
			"type":   "event_emitted",
			"params": map[string]any{"event_name": "transfer"},
		}
	}
	res, env := postJSON(t, "/monitors/1/rules/bulk", batch)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(env.Details) == 0 || env.Details[0].Field != "rules" {
		t.Fatalf("details = %+v, want field rules", env.Details)
	}
	if env.Details[0].Reason != "at most 50 rules per request" {
		t.Fatalf("reason = %q", env.Details[0].Reason)
	}
}

func TestCreateRulesBulk_MissingTypeAtIndex(t *testing.T) {
	res, env := postJSON(t, "/monitors/1/rules/bulk", []map[string]any{
		{"type": "event_emitted", "params": map[string]any{"event_name": "transfer"}},
		{"params": map[string]any{"event_name": "mint"}},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(env.Details) == 0 || env.Details[0].Field != "rules[1].type" {
		t.Fatalf("details = %+v, want rules[1].type", env.Details)
	}
}

func TestCreateRulesBulk_MaxSizeAccepted(t *testing.T) {
	st := &validationStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	batch := make([]map[string]any, maxBulkRules)
	for i := range batch {
		batch[i] = map[string]any{
			"type":   "event_emitted",
			"params": map[string]any{"event_name": fmt.Sprintf("e%d", i)},
		}
	}
	body, _ := json.Marshal(batch)
	res, err := http.Post(srv.URL+"/monitors/1/rules/bulk", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 201; body %s", res.StatusCode, raw)
	}
	if st.createdRules != maxBulkRules {
		t.Fatalf("created %d, want %d", st.createdRules, maxBulkRules)
	}
}
