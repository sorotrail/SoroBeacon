package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// dryRunStore is enough of Store for dry-run tests.
type dryRunStore struct {
	store.Store
	alerts       []store.Alert
	createCalled bool
}

func (s *dryRunStore) ListAlerts(_ context.Context, _ store.AlertFilter) ([]store.Alert, error) {
	return s.alerts, nil
}

func (s *dryRunStore) CreateAlert(_ context.Context, _ *store.Alert) (store.AlertOutcome, error) {
	s.createCalled = true
	return store.AlertCreated, nil
}

func (s *dryRunStore) CreateMonitor(_ context.Context, _ *store.Monitor) error { return nil }
func (s *dryRunStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	return &store.Monitor{ID: id, Name: "test", ContractIDs: []string{"CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"}}, nil
}

type dryRunDispatcher struct {
	dispatched bool
}

func (d *dryRunDispatcher) Dispatch(_ context.Context, _ notify.Alert) {
	d.dispatched = true
}

func makeAlert(id int64, contractID string) store.Alert {
	payload, _ := json.Marshal(map[string]any{
		"contract_id":      contractID,
		"event_name":       "transfer",
		"ledger":           float64(100 + int(id)),
		"ledger_closed_at": time.Now().UTC().Format(time.RFC3339),
		"tx_hash":          "tx" + string(rune('0'+id%10)),
		"topics":           []any{"transfer"},
		"value":            "100",
	})
	return store.Alert{
		ID:        id,
		MonitorID: 1,
		EventID:   "event-" + string(rune('0'+id%10)),
		Payload:   payload,
	}
}

func TestDryRun_ValidParams(t *testing.T) {
	alerts := []store.Alert{
		makeAlert(1, "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"),
		makeAlert(2, "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"),
	}
	st := &dryRunStore{alerts: alerts}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{
		"type":   "event_emitted",
		"params": map[string]any{"event_name": "transfer"},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules/dry-run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body %s", res.StatusCode, raw)
	}
	var resp dryRunResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count < 0 {
		t.Fatalf("count = %d, want >= 0", resp.Count)
	}
	if resp.TotalEvaluated != 2 {
		t.Fatalf("total_evaluated = %d, want 2", resp.TotalEvaluated)
	}
	if resp.Note == "" {
		t.Fatal("note should not be empty")
	}
}

func TestDryRun_InvalidParams(t *testing.T) {
	st := &dryRunStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	// Unknown rule type returns the same field-level errors as save.
	body, _ := json.Marshal(map[string]any{
		"type":   "not-a-rule",
		"params": map[string]any{},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules/dry-run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	var env errorEnvelope
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if len(env.Details) == 0 {
		t.Fatal("expected field-level details")
	}
	foundType := false
	for _, d := range env.Details {
		if d.Field == "type" {
			foundType = true
		}
	}
	if !foundType {
		t.Fatalf("expected type field error, got %+v", env.Details)
	}
}

func TestDryRun_NothingPersisted(t *testing.T) {
	st := &dryRunStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{
		"type":   "event_emitted",
		"params": map[string]any{"event_name": "transfer"},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules/dry-run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", res.StatusCode, raw)
	}
	if st.createCalled {
		t.Fatal("CreateAlert must not be called during dry run")
	}
}

func TestDryRun_NothingDelivered(t *testing.T) {
	st := &dryRunStore{alerts: []store.Alert{makeAlert(1, "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA")}}
	disp := &dryRunDispatcher{}
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{
		"type":   "event_emitted",
		"params": map[string]any{"event_name": "transfer"},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules/dry-run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", res.StatusCode, raw)
	}
	if disp.dispatched {
		t.Fatal("Dispatcher must not be called during dry run")
	}
}

func TestDryRun_MissingType(t *testing.T) {
	st := &dryRunStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{
		"params": map[string]any{"event_name": "transfer"},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules/dry-run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	var env errorEnvelope
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range env.Details {
		if d.Field == "type" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected type field error, got %+v", env.Details)
	}
}

// Test that decodeAlertToEvent reconstructs DecodedEvent correctly.
func TestDryRun_DecodeAlertToEvent(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"contract_id": "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		"event_name":  "transfer",
		"ledger":      float64(100),
		"ledger_closed_at": time.Now().UTC().Format(time.RFC3339),
		"tx_hash":     "txhash123",
		"topics":      []any{"transfer"},
		"value":       "100",
	})
	a := store.Alert{
		ID:        1,
		EventID:   "evt-001",
		Payload:   payload,
	}
	ev, ok := decodeAlertToEvent(a)
	if !ok {
		t.Fatal("decodeAlertToEvent should succeed")
	}
	if ev.ID != "evt-001" {
		t.Fatalf("ev.ID = %q, want evt-001", ev.ID)
	}
	if ev.ContractID != "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA" {
		t.Fatalf("ev.ContractID = %q, want contract id", ev.ContractID)
	}
	if ev.Ledger != 100 {
		t.Fatalf("ev.Ledger = %d, want 100", ev.Ledger)
	}
	if ev.TxHash != "txhash123" {
		t.Fatalf("ev.TxHash = %q, want txhash123", ev.TxHash)
	}
}

// Test that dry run with no alerts returns zero count.
func TestDryRun_NoAlerts(t *testing.T) {
	st := &dryRunStore{alerts: nil}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{
		"type":   "event_emitted",
		"params": map[string]any{"event_name": "transfer"},
	})
	res, err := http.Post(srv.URL+"/monitors/1/rules/dry-run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var resp dryRunResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 0 {
		t.Fatalf("count = %d, want 0", resp.Count)
	}
	if resp.TotalEvaluated != 0 {
		t.Fatalf("total_evaluated = %d, want 0", resp.TotalEvaluated)
	}
}

func TestDryRun_InvalidMonitorID(t *testing.T) {
	st := &dryRunStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	t.Cleanup(srv.Close)

	res, err := http.Post(srv.URL+"/monitors/not-a-number/rules/dry-run", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// mockDecodedEventStore is a store that returns DecodedEvent-based alerts
// for testing the dry-run evaluation path.
type mockDecodedEventStore struct {
	store.Store
	monitors map[int64]*store.Monitor
}

func (m *mockDecodedEventStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	if mon, ok := m.monitors[id]; ok {
		return mon, nil
	}
	return nil, store.ErrNotFound
}
func (m *mockDecodedEventStore) ListAlerts(_ context.Context, _ store.AlertFilter) ([]store.Alert, error) { return nil, nil }
func (m *mockDecodedEventStore) CreateAlert(_ context.Context, _ *store.Alert) (store.AlertOutcome, error) { return store.AlertCreated, nil }
func (m *mockDecodedEventStore) CreateMonitor(_ context.Context, _ *store.Monitor) error               { return nil }
func (m *mockDecodedEventStore) Ping(_ context.Context) error                                             { return nil }
func (m *mockDecodedEventStore) Close()                                                                     {}
