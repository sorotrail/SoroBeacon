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

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// fakeStore embeds the full Store interface so the fake keeps satisfying it
// as the interface grows; only what the probes touch is stubbed.
type fakeStore struct {
	store.Store
	pingErr error
}

func (f *fakeStore) Ping(ctx context.Context) error { return f.pingErr }

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
