package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// escalationStore is a store.Store fake covering just the escalation and
// channel lookups the handlers touch.
type escalationStore struct {
	store.Store
	policy        *store.EscalationPolicy
	channels      map[int64]store.Channel
	channelInUse  bool
	acknowledged  bool
	lastSetSteps  []store.EscalationStep
	policyDeleted bool
}

func (e *escalationStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	return &store.Monitor{ID: 1, Name: "m"}, nil
}

func (e *escalationStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	ch, ok := e.channels[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &ch, nil
}

func (e *escalationStore) GetEscalationPolicyForMonitor(context.Context, int64) (*store.EscalationPolicy, error) {
	if e.policy == nil {
		return nil, store.ErrNotFound
	}
	return e.policy, nil
}

func (e *escalationStore) SetEscalationPolicy(_ context.Context, monitorID int64, steps []store.EscalationStep) (*store.EscalationPolicy, error) {
	e.lastSetSteps = steps
	e.policy = &store.EscalationPolicy{ID: 7, MonitorID: monitorID, Steps: steps, CreatedAt: time.Unix(0, 0).UTC()}
	return e.policy, nil
}

func (e *escalationStore) DeleteEscalationPolicy(context.Context, int64) error {
	if e.policy == nil {
		return store.ErrNotFound
	}
	e.policyDeleted = true
	e.policy = nil
	return nil
}

func (e *escalationStore) AcknowledgeAlert(context.Context, int64) error {
	e.acknowledged = true
	return nil
}

func (e *escalationStore) GetAlert(context.Context, int64) (*store.Alert, error) {
	now := time.Unix(1_700_000_000, 0).UTC()
	return &store.Alert{ID: 5, MonitorID: 1, AcknowledgedAt: &now}, nil
}

func (e *escalationStore) DeleteChannel(context.Context, int64) error {
	if e.channelInUse {
		return store.ErrChannelInUse
	}
	return nil
}

func escalationServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).Routes())
	t.Cleanup(srv.Close)
	return srv
}

func doRequest(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestPutEscalationStoresOrderedSteps(t *testing.T) {
	st := &escalationStore{channels: map[int64]store.Channel{1: {ID: 1}, 2: {ID: 2}}}
	srv := escalationServer(t, st)

	status, body := doRequest(t, http.MethodPut, srv.URL+"/monitors/1/escalation",
		`{"steps":[{"delay":"5m","channel_ids":[1]},{"delay":"30m","channel_ids":[2]}]}`)
	if status != http.StatusOK {
		t.Fatalf("PUT = %d (%v), want 200", status, body)
	}
	steps, _ := body["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps = %v, want 2", body["steps"])
	}
	first, _ := steps[0].(map[string]any)
	if first["delay"] != "5m0s" {
		t.Fatalf("first step delay = %v, want 5m0s", first["delay"])
	}
	if first["position"].(float64) != 0 {
		t.Fatalf("first step position = %v, want 0", first["position"])
	}
	if len(st.lastSetSteps) != 2 || st.lastSetSteps[0].DelaySeconds != 300 {
		t.Fatalf("stored steps = %+v, want 300s first", st.lastSetSteps)
	}
}

func TestPutEscalationRejectsMissingSteps(t *testing.T) {
	srv := escalationServer(t, &escalationStore{channels: map[int64]store.Channel{}})
	status, body := doRequest(t, http.MethodPut, srv.URL+"/monitors/1/escalation", `{"steps":[]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400", status)
	}
	if body["error"] == nil {
		t.Fatalf("expected an error envelope, got %v", body)
	}
}

func TestPutEscalationRejectsUnknownChannel(t *testing.T) {
	st := &escalationStore{channels: map[int64]store.Channel{1: {ID: 1}}}
	srv := escalationServer(t, st)

	status, body := doRequest(t, http.MethodPut, srv.URL+"/monitors/1/escalation",
		`{"steps":[{"delay":"1m","channel_ids":[1,999]}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400", status)
	}
	details, _ := body["details"].([]any)
	if len(details) != 1 {
		t.Fatalf("details = %v, want one entry", body["details"])
	}
	d, _ := details[0].(map[string]any)
	if d["field"] != "steps[0].channel_ids[1]" {
		t.Fatalf("field = %v, want steps[0].channel_ids[1]", d["field"])
	}
}

func TestPutEscalationRejectsNegativeDelay(t *testing.T) {
	srv := escalationServer(t, &escalationStore{channels: map[int64]store.Channel{1: {ID: 1}}})
	status, _ := doRequest(t, http.MethodPut, srv.URL+"/monitors/1/escalation",
		`{"steps":[{"delay":"-1m","channel_ids":[1]}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400", status)
	}
}

func TestGetEscalationNotFoundWhenNone(t *testing.T) {
	srv := escalationServer(t, &escalationStore{channels: map[int64]store.Channel{}})
	status, _ := doRequest(t, http.MethodGet, srv.URL+"/monitors/1/escalation", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET = %d, want 404", status)
	}
}

func TestDeleteEscalationNotFoundWhenNone(t *testing.T) {
	srv := escalationServer(t, &escalationStore{channels: map[int64]store.Channel{}})
	status, _ := doRequest(t, http.MethodDelete, srv.URL+"/monitors/1/escalation", "")
	if status != http.StatusNotFound {
		t.Fatalf("DELETE = %d, want 404", status)
	}
}

func TestDeleteEscalationRemovesPolicy(t *testing.T) {
	st := &escalationStore{channels: map[int64]store.Channel{}, policy: &store.EscalationPolicy{ID: 7, MonitorID: 1}}
	srv := escalationServer(t, st)
	status, _ := doRequest(t, http.MethodDelete, srv.URL+"/monitors/1/escalation", "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", status)
	}
	if !st.policyDeleted {
		t.Fatal("policy was not deleted")
	}
}

func TestAcknowledgeAlert(t *testing.T) {
	st := &escalationStore{channels: map[int64]store.Channel{}}
	srv := escalationServer(t, st)

	status, body := doRequest(t, http.MethodPost, srv.URL+"/alerts/5/acknowledge", "")
	if status != http.StatusOK {
		t.Fatalf("POST acknowledge = %d, want 200", status)
	}
	if !st.acknowledged {
		t.Fatal("alert was not acknowledged")
	}
	if body["acknowledged_at"] == nil {
		t.Fatalf("acknowledged_at missing from response: %v", body)
	}
}

func TestDeleteChannelInUseReturnsConflict(t *testing.T) {
	st := &escalationStore{channels: map[int64]store.Channel{}, channelInUse: true}
	srv := escalationServer(t, st)
	status, body := doRequest(t, http.MethodDelete, srv.URL+"/channels/9", "")
	if status != http.StatusConflict {
		t.Fatalf("DELETE channel = %d, want 409", status)
	}
	if body["error"] == nil {
		t.Fatalf("expected an error envelope, got %v", body)
	}
}
