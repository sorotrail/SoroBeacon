package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type stubPosition struct{ pos poller.Position }

func (s stubPosition) Position() poller.Position { return s.pos }

// fakeStore embeds the full Store interface so the fake keeps satisfying it
// as the interface grows; only what the probes touch is stubbed.
type fakeStore struct {
	store.Store
	pingErr error
}

func (f *fakeStore) Ping(ctx context.Context) error { return f.pingErr }
func (f *fakeStore) ListSavedSearches(context.Context) ([]store.SavedSearch, error) {
	return nil, nil
}
func (f *fakeStore) CreateSavedSearch(context.Context, *store.SavedSearch) error { return nil }
func (f *fakeStore) GetSavedSearch(context.Context, int64) (*store.SavedSearch, error) {
	return nil, store.ErrNotFound
}
func (f *fakeStore) DeleteSavedSearch(context.Context, int64) error                      { return nil }
func (f *fakeStore) SetDefaultSearch(context.Context, int64) error                       { return nil }
func (f *fakeStore) ClearDefaultSearch(context.Context, int64) error                     { return nil }
func (f *fakeStore) CreateMonitorTemplate(context.Context, *store.MonitorTemplate) error { return nil }
func (f *fakeStore) GetMonitorTemplate(context.Context, int64) (*store.MonitorTemplate, error) {
	return nil, store.ErrNotFound
}
func (f *fakeStore) ListMonitorTemplates(context.Context) ([]store.MonitorTemplate, error) {
	return nil, nil
}
func (f *fakeStore) UpdateMonitorTemplate(context.Context, *store.MonitorTemplate) error { return nil }
func (f *fakeStore) DeleteMonitorTemplate(context.Context, int64) error                  { return nil }
func (f *fakeStore) ListMonitors(context.Context, bool) ([]store.Monitor, error) {
	return nil, nil
}

// fakeRPC implements stellar.Client; the probes use GetHealth only.
type fakeRPC struct {
	healthErr error
}

func (f *fakeRPC) GetEvents(ctx context.Context, req stellar.GetEventsRequest) (*stellar.GetEventsResult, error) {
	return &stellar.GetEventsResult{}, nil
}
func (f *fakeRPC) GetLatestLedger(ctx context.Context) (*stellar.LatestLedger, error) {
	return &stellar.LatestLedger{}, nil
}
func (f *fakeRPC) GetHealth(ctx context.Context) (*stellar.Health, error) {
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return &stellar.Health{LatestLedger: 1234}, nil
}
func (f *fakeRPC) GetNetwork(ctx context.Context) (*stellar.Network, error) {
	return &stellar.Network{}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

func newProbeServer(st store.Store, rpc stellar.Client) chi.Router {
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), rpc, discardLogger())
	return s.Routes()
}

// livez must stay 200 while every dependency is down: a liveness probe that
// fails on dependencies makes an orchestrator restart-loop a recoverable
// instance instead of pulling it from the load balancer.
func TestLivezHealthyWhenDependenciesDown(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(
		&fakeStore{pingErr: errors.New("db down")},
		&fakeRPC{healthErr: errors.New("rpc down")}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("livez with both dependencies down = %d, want 200", res.StatusCode)
	}
}

func TestReadyzAllHealthy(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(&fakeStore{}, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readyz with healthy deps = %d, want 200", res.StatusCode)
	}

	var body struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Healthy bool   `json:"healthy"`
			Detail  string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ready" {
		t.Fatalf("status = %q, want ready", body.Status)
	}
	if !body.Checks["database"].Healthy || !body.Checks["rpc"].Healthy {
		t.Fatalf("checks = %+v, want both healthy", body.Checks)
	}
}

func TestReadyzReportsWhichDependencyFailed(t *testing.T) {
	srv := httptest.NewServer(newProbeServer(
		&fakeStore{pingErr: errors.New("connection refused")},
		&fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with db down = %d, want 503", res.StatusCode)
	}

	var body struct {
		Checks map[string]struct {
			Healthy bool   `json:"healthy"`
			Detail  string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Checks["database"].Healthy {
		t.Fatal("database check should be unhealthy")
	}
	if body.Checks["database"].Detail == "" {
		t.Fatal("failed check should carry a detail string")
	}
	if !body.Checks["rpc"].Healthy {
		t.Fatal("rpc check should still be healthy — one bad dependency must not fail the rest")
	}
}

func TestVersionEndpoint(t *testing.T) {
	// Pin the real values, not overrides, so a broken ldflags path shows
	// up as "dev"/"none" rather than silently passing.
	srv := httptest.NewServer(newProbeServer(&fakeStore{}, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("version = %d, want 200", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "commit", "build_date"} {
		if body[key] == "" {
			t.Fatalf("version response missing %q: %+v", key, body)
		}
	}
}

func decodeHealth(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, body
}

func TestHealthOmitsPollerFieldsBeforeFirstPoll(t *testing.T) {
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithPoller(stubPosition{})
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	code, body := decodeHealth(t, srv.URL+"/health")
	if code != http.StatusOK {
		t.Fatalf("health = %d, want 200", code)
	}
	for _, key := range []string{"last_processed_ledger", "latest_chain_ledger", "ledger_lag", "last_successful_poll"} {
		if _, ok := body[key]; ok {
			t.Fatalf("pre-poll health must omit %q, got %+v", key, body)
		}
	}
}

func TestHealthIncludesPollerFieldsWhenReady(t *testing.T) {
	at := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithPoller(stubPosition{pos: poller.Position{
			LastProcessedLedger: 100,
			LatestChainLedger:   110,
			LastSuccessfulPoll:  at,
		}})
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	code, body := decodeHealth(t, srv.URL+"/health")
	if code != http.StatusOK {
		t.Fatalf("health = %d, want 200", code)
	}
	if body["last_processed_ledger"] != float64(100) {
		t.Fatalf("last_processed_ledger = %v", body["last_processed_ledger"])
	}
	if body["latest_chain_ledger"] != float64(110) {
		t.Fatalf("latest_chain_ledger = %v", body["latest_chain_ledger"])
	}
	if body["ledger_lag"] != float64(10) {
		t.Fatalf("ledger_lag = %v", body["ledger_lag"])
	}
	if body["last_successful_poll"] != "2026-09-22T00:00:00Z" {
		t.Fatalf("last_successful_poll = %v", body["last_successful_poll"])
	}
}

func TestPollerStatusReportsPositionWithoutConfiguration(t *testing.T) {
	at := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithPoller(stubPosition{pos: poller.Position{
			LastProcessedLedger: 100,
			LatestChainLedger:   110,
			LastSuccessfulPoll:  at,
			BackingOff:          true,
		}})
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	code, body := decodeHealth(t, srv.URL+"/poller")
	if code != http.StatusOK {
		t.Fatalf("poller status = %d, want 200", code)
	}
	if body["status"] != "ok" || body["last_processed_ledger"] != float64(100) ||
		body["latest_chain_ledger"] != float64(110) || body["ledger_lag"] != float64(10) ||
		body["last_successful_poll"] != "2026-09-22T00:00:00Z" || body["backing_off"] != true {
		t.Fatalf("poller status = %+v", body)
	}
	for _, key := range []string{"rpc_url", "database_url", "config"} {
		if _, ok := body[key]; ok {
			t.Fatalf("poller status must not expose %q: %+v", key, body)
		}
	}
}

func TestPollerStatusWaitsBeforeFirstPoll(t *testing.T) {
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithPoller(stubPosition{})
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	code, body := decodeHealth(t, srv.URL+"/poller")
	if code != http.StatusOK || body["status"] != "waiting" || body["backing_off"] != false {
		t.Fatalf("pre-poll status = %d, %+v", code, body)
	}
	for _, key := range []string{"last_processed_ledger", "latest_chain_ledger", "ledger_lag", "last_successful_poll"} {
		if _, ok := body[key]; ok {
			t.Fatalf("pre-poll status must omit %q: %+v", key, body)
		}
	}
}

func TestReadyzLagThresholdDisabledByDefault(t *testing.T) {
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithPoller(stubPosition{pos: poller.Position{
			LastProcessedLedger: 1,
			LatestChainLedger:   10_000,
			LastSuccessfulPoll:  time.Now(),
		}})
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readyz with lag but threshold disabled = %d, want 200", res.StatusCode)
	}
}

func TestReadyzFailsWhenLagExceedsThreshold(t *testing.T) {
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithPoller(stubPosition{pos: poller.Position{
			LastProcessedLedger: 100,
			LatestChainLedger:   200,
			LastSuccessfulPoll:  time.Now(),
		}}).
		WithReadyzLagThreshold(50)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz lagging = %d, want 503", res.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Healthy bool   `json:"healthy"`
			Detail  string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Checks["poller"].Healthy {
		t.Fatalf("poller check should be unhealthy: %+v", body.Checks["poller"])
	}
	if body.Checks["database"].Healthy == false || body.Checks["rpc"].Healthy == false {
		t.Fatalf("db/rpc should stay healthy: %+v", body.Checks)
	}
}

func TestReadyzHealthyWhenLagWithinThreshold(t *testing.T) {
	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithPoller(stubPosition{pos: poller.Position{
			LastProcessedLedger: 100,
			LatestChainLedger:   110,
			LastSuccessfulPoll:  time.Now(),
		}}).
		WithReadyzLagThreshold(50)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readyz within threshold = %d, want 200", res.StatusCode)
	}
}
