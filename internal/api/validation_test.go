package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

const validContract = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

// validationStore is enough of Store for create/update validation tests.
// Persistence is not under test; we only need the handlers to accept a
// well-formed body without panicking on the interface embed.
type validationStore struct {
	store.Store
	createdMonitors int
	createdRules    int
	createdChannels int
}

func (v *validationStore) CreateMonitor(_ context.Context, m *store.Monitor) error {
	v.createdMonitors++
	m.ID = int64(v.createdMonitors)
	return nil
}

func (v *validationStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	return &store.Monitor{ID: id, Name: "existing", ContractIDs: []string{validContract}, Enabled: true}, nil
}

func (v *validationStore) CreateRule(_ context.Context, r *store.Rule) error {
	v.createdRules++
	r.ID = int64(v.createdRules)
	return nil
}

func (v *validationStore) CreateChannel(_ context.Context, ch *store.Channel) error {
	v.createdChannels++
	ch.ID = int64(v.createdChannels)
	return nil
}

type errorEnvelope struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
	Details   []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	} `json:"details"`
}

func postJSON(t *testing.T, path string, body any) (*http.Response, errorEnvelope) {
	t.Helper()
	st := &validationStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	res, err := http.Post(srv.URL+path, "application/json", rdr)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode %s: %v\nbody: %s", path, err, raw)
	}
	return res, env
}

func TestCreateMonitor_MultipleValidationDetails(t *testing.T) {
	res, env := postJSON(t, "/monitors", map[string]any{
		"name":         "",
		"contract_ids": []string{},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if env.Error != "validation failed" {
		t.Fatalf("error = %q, want validation failed", env.Error)
	}
	if env.Code != "Bad Request" {
		t.Fatalf("code = %q, want Bad Request", env.Code)
	}
	got := map[string]string{}
	for _, d := range env.Details {
		got[d.Field] = d.Reason
	}
	if got["name"] != "name is required" {
		t.Fatalf("name detail = %q", got["name"])
	}
	if got["contract_ids"] != "contract_ids is required" {
		t.Fatalf("contract_ids detail = %q", got["contract_ids"])
	}
	if len(env.Details) != 2 {
		t.Fatalf("details = %+v, want 2", env.Details)
	}
}

func TestCreateMonitor_InvalidContractIDsAreIndexed(t *testing.T) {
	res, env := postJSON(t, "/monitors", map[string]any{
		"name":         "ops",
		"contract_ids": []string{"not-a-contract", "also-bad"},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(env.Details) != 2 {
		t.Fatalf("details = %+v, want 2 indexed contract_ids", env.Details)
	}
	if env.Details[0].Field != "contract_ids[0]" || env.Details[1].Field != "contract_ids[1]" {
		t.Fatalf("fields = %q, %q", env.Details[0].Field, env.Details[1].Field)
	}
}

func TestCreateMonitor_ValidRequestUnaffected(t *testing.T) {
	st := &validationStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"name":         "ops",
		"contract_ids": []string{validContract},
	})
	res, err := http.Post(srv.URL+"/monitors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 201; body %s", res.StatusCode, raw)
	}
	if st.createdMonitors != 1 {
		t.Fatalf("created %d monitors, want 1", st.createdMonitors)
	}
	var m store.Monitor
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "ops" || m.ID == 0 {
		t.Fatalf("monitor = %+v", m)
	}
}

func TestCreateRule_MultipleParamDetails(t *testing.T) {
	res, env := postJSON(t, "/monitors/1/rules", map[string]any{
		"type": "token_event",
		"params": map[string]any{
			"event":      "freeze",
			"min_amount": "x",
			"max_amount": "y",
		},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if env.Error != "validation failed" {
		t.Fatalf("error = %q, want validation failed", env.Error)
	}
	got := map[string]string{}
	for _, d := range env.Details {
		got[d.Field] = d.Reason
	}
	if _, ok := got["params.event"]; !ok {
		t.Fatalf("missing params.event in %+v", env.Details)
	}
	if _, ok := got["params.min_amount"]; !ok {
		t.Fatalf("missing params.min_amount in %+v", env.Details)
	}
	if _, ok := got["params.max_amount"]; !ok {
		t.Fatalf("missing params.max_amount in %+v", env.Details)
	}
}

func TestCreateRule_ValidRequestUnaffected(t *testing.T) {
	st := &validationStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"type":   "token_event",
		"params": map[string]any{"event": "transfer"},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 201; body %s", res.StatusCode, raw)
	}
	if st.createdRules != 1 {
		t.Fatalf("created %d rules, want 1", st.createdRules)
	}
}

func TestCreateChannel_MultipleValidationDetails(t *testing.T) {
	res, env := postJSON(t, "/channels", map[string]any{
		"name": "",
		"type": "",
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	got := map[string]string{}
	for _, d := range env.Details {
		got[d.Field] = d.Reason
	}
	if got["name"] != "name is required" || got["type"] != "type is required" {
		t.Fatalf("details = %+v", env.Details)
	}
}

func TestCreateMonitor_SingleDetailKeepsTopLevelError(t *testing.T) {
	res, env := postJSON(t, "/monitors", map[string]any{
		"contract_ids": []string{validContract},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if env.Error != "name is required" {
		t.Fatalf("error = %q, want name is required (single-detail clients)", env.Error)
	}
	if len(env.Details) != 1 || env.Details[0].Field != "name" {
		t.Fatalf("details = %+v", env.Details)
	}
}

func TestWriteErrOmitsDetailsWhenEmpty(t *testing.T) {
	st := &validationStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/monitors/not-a-number")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["details"]; ok {
		t.Fatalf("details present on a non-validation error: %s", raw)
	}
	if body["error"] != "invalid id" {
		t.Fatalf("error = %v", body["error"])
	}
}
