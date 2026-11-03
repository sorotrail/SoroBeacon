package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/apierr"
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

func (v *validationStore) CreateRules(_ context.Context, rules []*store.Rule) error {
	for _, r := range rules {
		v.createdRules++
		r.ID = int64(v.createdRules)
	}
	return nil
}

func (v *validationStore) CreateChannel(_ context.Context, ch *store.Channel) error {
	v.createdChannels++
	ch.ID = int64(v.createdChannels)
	return nil
}

func (v *validationStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	return &store.Channel{
		ID:      id,
		Name:    "existing",
		Type:    "slack",
		Config:  json.RawMessage(`{"webhook_url":"https://hooks.slack.com/services/T/B/X"}`),
		Enabled: true,
	}, nil
}

func (v *validationStore) UpdateChannel(_ context.Context, _ *store.Channel) error {
	return nil
}

func (v *validationStore) SetMonitorsEnabled(_ context.Context, ids []int64, _ bool) (int, []int64, error) {
	return len(ids), []int64{}, nil
}

func (v *validationStore) CreateAuditEntry(_ context.Context, _ *store.AuditEntry) error {
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
	if env.Code != apierr.CodeInvalidArgument {
		t.Fatalf("code = %q, want %q", env.Code, apierr.CodeInvalidArgument)
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

// TestCreateChannel_RejectsInvalidTemplate proves a template with a syntax
// error is caught when the channel is created (a 400 naming the parse error),
// rather than at delivery time.
func TestCreateChannel_RejectsInvalidTemplate(t *testing.T) {
	res, env := postJSON(t, "/channels", map[string]any{
		"name": "ops-slack",
		"type": "slack",
		"config": map[string]any{
			"webhook_url": "https://hooks.slack.com/services/T/B/X",
			"template":    "{{.MonitorName",
		},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(env.Details) != 1 || env.Details[0].Field != "config" {
		t.Fatalf("details = %+v", env.Details)
	}
	if !strings.Contains(env.Details[0].Reason, "invalid template") {
		t.Fatalf("reason = %q, want the parse error", env.Details[0].Reason)
	}
}

// TestCreateRule_RejectsInvalidCooldown proves the cross-cutting cooldown is
// validated when the rule is created, not discovered when a burst starts.
func TestCreateRule_RejectsInvalidCooldown(t *testing.T) {
	res, env := postJSON(t, "/monitors/1/rules", map[string]any{
		"type":   "event_emitted",
		"params": map[string]any{"event_name": "transfer", "cooldown": "5 minutes"},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(env.Details) != 1 || env.Details[0].Field != "params.cooldown" {
		t.Fatalf("details = %+v, want a params.cooldown detail", env.Details)
	}
}

// TestUpdateChannel_RejectsInvalidTemplate covers the other write path: a bad
// template sent on update is rejected before it is persisted.
func TestUpdateChannel_RejectsInvalidTemplate(t *testing.T) {
	st := &validationStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	body, err := json.Marshal(map[string]any{
		"config": map[string]any{
			"webhook_url": "https://hooks.slack.com/services/T/B/X",
			"template":    "{{.MonitorName",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/channels/1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", res.StatusCode, raw)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, raw)
	}
	if len(env.Details) != 1 || !strings.Contains(env.Details[0].Reason, "invalid template") {
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

func TestCreateChannel_TimeoutValidation(t *testing.T) {
	cases := []struct {
		name    string
		timeout any
	}{
		{"zero", 0},
		{"negative", -5},
		{"exceeds ceiling number", 61},
		{"ten minutes number", 600},
		{"duration exceeding ceiling", "10m"},
		{"duration zero", "0s"},
		{"invalid duration string", "not-a-duration"},
		{"invalid boolean type", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, env := postJSON(t, "/channels", map[string]any{
				"name": "ops-hook",
				"type": "webhook",
				"config": map[string]any{
					"url":    "https://example.com/webhook",
					"secret": "s3cret",
				},
				"timeout": tc.timeout,
			})
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			found := false
			for _, d := range env.Details {
				if d.Field == "timeout" {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected Field: timeout in details, got %+v", env.Details)
			}
		})
	}
}

func TestCreateChannel_ValidTimeout(t *testing.T) {
	cases := []struct {
		name        string
		timeout     any
		wantTimeout int
	}{
		{"omitted", nil, 15},
		{"explicit integer", 30, 30},
		{"duration string", "45s", 45},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"name": "ops-hook",
				"type": "webhook",
				"config": map[string]any{
					"url":    "https://example.com/webhook",
					"secret": "s3cret",
				},
			}
			if tc.timeout != nil {
				payload["timeout"] = tc.timeout
			}

			st := &validationStore{}
			srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
			defer srv.Close()

			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			res, err := http.Post(srv.URL+"/channels", "application/json", bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusCreated {
				raw, _ := io.ReadAll(res.Body)
				t.Fatalf("status = %d, want 201; body: %s", res.StatusCode, raw)
			}

			var ch store.Channel
			if err := json.NewDecoder(res.Body).Decode(&ch); err != nil {
				t.Fatal(err)
			}
			if ch.TimeoutSeconds() != tc.wantTimeout {
				t.Fatalf("timeout = %d, want %d", ch.TimeoutSeconds(), tc.wantTimeout)
			}
		})
	}
}

func TestUpdateChannel_TimeoutValidation(t *testing.T) {
	t.Run("rejects invalid timeout on update", func(t *testing.T) {
		st := &validationStore{}
		srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
		defer srv.Close()

		body, _ := json.Marshal(map[string]any{"timeout": 600})
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/channels/1", bytes.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.StatusCode)
		}
		var env errorEnvelope
		_ = json.NewDecoder(res.Body).Decode(&env)
		found := false
		for _, d := range env.Details {
			if d.Field == "timeout" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected Field: timeout in details, got %+v", env.Details)
		}
	})

	t.Run("accepts valid timeout on update", func(t *testing.T) {
		st := &validationStore{}
		srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
		defer srv.Close()

		body, _ := json.Marshal(map[string]any{"timeout": 25})
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/channels/1", bytes.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(res.Body)
			t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, raw)
		}
		var ch store.Channel
		if err := json.NewDecoder(res.Body).Decode(&ch); err != nil {
			t.Fatal(err)
		}
		if ch.TimeoutSeconds() != 25 {
			t.Fatalf("timeout = %d, want 25", ch.TimeoutSeconds())
		}
	})
}

