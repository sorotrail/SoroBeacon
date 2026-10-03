package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// networkStore records the monitor rows writes would persist and the filters
// listings would apply, so a test can assert on what the handler decided
// rather than on a database.
type networkStore struct {
	store.Store
	created  store.Monitor
	updated  store.Monitor
	gotAlert store.AlertFilter
	getErr   error
	existing *store.Monitor
}

func (n *networkStore) CreateMonitor(ctx context.Context, m *store.Monitor) error {
	m.ID = 1
	n.created = *m
	return nil
}

func (n *networkStore) GetMonitor(ctx context.Context, id int64) (*store.Monitor, error) {
	if n.getErr != nil {
		return nil, n.getErr
	}
	if n.existing != nil {
		return n.existing, nil
	}
	return &store.Monitor{ID: id, Name: "existing", Network: "testnet", ContractIDs: []string{"C"}}, nil
}

func (n *networkStore) UpdateMonitor(ctx context.Context, m *store.Monitor) error {
	n.updated = *m
	return nil
}

func (n *networkStore) ListMonitorsPage(ctx context.Context, f store.ListFilter) ([]store.Monitor, error) {
	return nil, nil
}

func (n *networkStore) ListAlerts(ctx context.Context, f store.AlertFilter) ([]store.Alert, error) {
	n.gotAlert = f
	return nil, nil
}

func (n *networkStore) Ping(ctx context.Context) error { return nil }

func newNetworkServer(st store.Store, networks []string, p PositionReader) chi.Router {
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithNetworks(networks)
	if p != nil {
		s = s.WithPoller(p)
	}
	return s.Routes()
}

func postJSONTo(t *testing.T, h http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	var out map[string]any
	if res.Body.Len() > 0 {
		// A non-JSON body (204, or an empty error) is fine to ignore.
		_ = json.Unmarshal(res.Body.Bytes(), &out)
	}
	return res.Code, out
}

// TestCreateMonitor_NetworkDefaultingAndValidation: a monitor must be born on
// a chain this instance actually polls. Defaulting to the primary keeps every
// single-network body working unchanged; rejecting an unknown name is what
// stops a typo from stranding a monitor that never alerts.
func TestCreateMonitor_NetworkDefaultingAndValidation(t *testing.T) {
	tests := []struct {
		name        string
		networks    []string
		body        string
		wantStatus  int
		wantNetwork string
		wantErr     string
	}{
		{
			name:        "omitted means the primary",
			networks:    []string{"testnet", "mainnet"},
			body:        `{"name":"m","contract_ids":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"]}`,
			wantStatus:  http.StatusCreated,
			wantNetwork: "testnet",
		},
		{
			name:        "a configured network is accepted",
			networks:    []string{"testnet", "mainnet"},
			body:        `{"name":"m","contract_ids":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],"network":"MAINNET"}`,
			wantStatus:  http.StatusCreated,
			wantNetwork: "mainnet",
		},
		{
			name:       "an unpolled chain is rejected",
			networks:   []string{"testnet", "mainnet"},
			body:       `{"name":"m","contract_ids":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],"network":"futurenet"}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "does not poll",
		},
		{
			name:       "an empty network is not the same as omitting it",
			networks:   []string{"testnet"},
			body:       `{"name":"m","contract_ids":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],"network":"  "}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "must not be empty",
		},
		{
			// With no list configured the instance is the pre-multi-network
			// shape; a body must not invent a chain nobody polls.
			name:       "no configured networks",
			networks:   nil,
			body:       `{"name":"m","contract_ids":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],"network":"testnet"}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "polls one unnamed network",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &networkStore{}
			h := newNetworkServer(st, tt.networks, nil)
			code, body := postJSONTo(t, h, "/monitors", tt.body)
			if code != tt.wantStatus {
				t.Fatalf("status = %d body=%v, want %d", code, body, tt.wantStatus)
			}
			if tt.wantErr != "" {
				if !strings.Contains(resumeJSON(body), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", resumeJSON(body), tt.wantErr)
				}
				return
			}
			if st.created.Network != tt.wantNetwork {
				t.Fatalf("stored network = %q, want %q", st.created.Network, tt.wantNetwork)
			}
		})
	}
}

// resumeJSON flattens a validation body for substring assertions.
func resumeJSON(body map[string]any) string {
	raw, _ := json.Marshal(body)
	return string(raw)
}

// TestUpdateMonitor_RejectsNetworkChange: contract IDs are only meaningful on
// the chain they were deployed to, so re-homing a monitor would silently
// repoint every rule at a different contract with the same address.
func TestUpdateMonitor_RejectsNetworkChange(t *testing.T) {
	tests := []struct {
		name       string
		networks   []string
		body       string
		wantStatus int
		wantErr    string
		unchanged  bool
	}{
		{
			name:       "a different chain is refused",
			networks:   []string{"testnet", "mainnet"},
			body:       `{"network":"mainnet"}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "network is fixed",
		},
		{
			name:       "restating the current chain is a no-op",
			networks:   []string{"testnet", "mainnet"},
			body:       `{"network":"testnet","name":"renamed"}`,
			wantStatus: http.StatusOK,
			unchanged:  true,
		},
		{
			name:       "an unknown chain is refused",
			networks:   []string{"testnet"},
			body:       `{"network":"futurenet"}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "network is fixed",
		},
		{
			name:       "omitting network leaves it alone",
			networks:   []string{"testnet", "mainnet"},
			body:       `{"name":"renamed"}`,
			wantStatus: http.StatusOK,
			unchanged:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &networkStore{existing: &store.Monitor{
				ID: 7, Name: "existing", Network: "testnet", ContractIDs: []string{"C"},
			}}
			h := newNetworkServer(st, tt.networks, nil)
			req := httptest.NewRequest(http.MethodPatch, "/monitors/7", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			if res.Code != tt.wantStatus {
				t.Fatalf("status = %d body=%s, want %d", res.Code, res.Body.String(), tt.wantStatus)
			}
			if tt.wantErr != "" && !strings.Contains(res.Body.String(), tt.wantErr) {
				t.Fatalf("body %q does not contain %q", res.Body.String(), tt.wantErr)
			}
			if tt.wantErr == "" && st.updated.Network != "testnet" {
				t.Fatalf("stored network = %q, want the monitor's own %q", st.updated.Network, "testnet")
			}
		})
	}
}

// TestListFilters_CarryNetworkToTheStore checks the query parameters actually
// reach the store: a filter dropped on the floor reads as "no results", which
// an operator would blame on the chain rather than the handler.
func TestListFilters_CarryNetworkToTheStore(t *testing.T) {
	st := &networkStore{}
	h := newNetworkServer(st, []string{"testnet", "mainnet"}, nil)

	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/monitors?network=mainnet", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("monitors status = %d", res.Code)
	}

	res = httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/alerts?network=testnet", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("alerts status = %d body=%s", res.Code, res.Body.String())
	}
	if st.gotAlert.Network != "testnet" {
		t.Fatalf("alert filter network = %q, want testnet", st.gotAlert.Network)
	}
}

// statusesReader is a supervisor stand-in: one unit per network, each reporting
// whatever the test needs.
type statusesReader struct {
	statuses []poller.NetworkStatus
	pos      poller.Position
}

func (s *statusesReader) Position() poller.Position { return s.pos }
func (s *statusesReader) Statuses(context.Context) []poller.NetworkStatus {
	return s.statuses
}

// TestHealth_ReportsEveryNetwork: collapsing multi-chain ingest into one
// boolean is what makes an instance that lost a chain look healthy. The
// per-network array is the point of the endpoint here.
func TestHealth_ReportsEveryNetwork(t *testing.T) {
	r := &statusesReader{statuses: []poller.NetworkStatus{
		{Network: "testnet", Source: "ok", LastProcessedLedger: 100, LatestChainLedger: 101, LedgerLag: 1},
		{Network: "mainnet", Source: "rpc unreachable", LastProcessedLedger: 500},
	}}
	srv := httptest.NewServer(newNetworkServer(&fakeStore{}, []string{"testnet", "mainnet"}, r))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "degraded" {
		t.Fatalf("status = %v, want degraded because one network's source is failing", body["status"])
	}
	list, ok := body["networks"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("networks = %v, want one entry per chain", body["networks"])
	}
	first := list[0].(map[string]any)
	if first["network"] != "testnet" || first["source"] != "ok" {
		t.Fatalf("networks[0] = %v", first)
	}
	second := list[1].(map[string]any)
	if second["source"] != "rpc unreachable" {
		t.Fatalf("networks[1] = %v, want the failing chain's error reported", second)
	}
}

// TestHealth_NoNetworksArrayForSingleChain: an instance that polls one network
// has nothing to break out, and its probes must answer exactly as they did
// before multi-network ingestion existed.
func TestHealth_NoNetworksArrayForSingleChain(t *testing.T) {
	srv := httptest.NewServer(newNetworkServer(&fakeStore{}, nil, stubPosition{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["networks"]; ok {
		t.Fatalf("single-chain health body has a networks field: %v", body["networks"])
	}
}

// TestReadyz_ReportsOneCheckPerNetwork: the aggregate `rpc` key stays for
// existing probe configs, and each chain gets its own so an orchestrator (or a
// human reading the body) can see which one is behind.
func TestReadyz_ReportsOneCheckPerNetwork(t *testing.T) {
	r := &statusesReader{statuses: []poller.NetworkStatus{
		{Network: "testnet", Source: "ok", LatestChainLedger: 42},
		{Network: "mainnet", Source: "timeout", LatestChainLedger: 7},
	}}
	srv := httptest.NewServer(newNetworkServer(&fakeStore{}, []string{"testnet", "mainnet"}, r))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		Checks map[string]struct {
			Healthy bool   `json:"healthy"`
			Detail  string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body.Checks["rpc"]; !ok {
		t.Fatalf("checks = %v, want the pre-existing rpc key kept", body.Checks)
	}
	if got := body.Checks["rpc_testnet"]; !got.Healthy || !strings.Contains(got.Detail, "42") {
		t.Fatalf("rpc_testnet = %+v, want healthy with the chain's reported ledger", got)
	}
	if got := body.Checks["rpc_mainnet"]; got.Healthy || !strings.Contains(got.Detail, "timeout") {
		t.Fatalf("rpc_mainnet = %+v, want the failing chain's error", got)
	}
	if !body.Checks["rpc"].Healthy {
		t.Fatalf("rpc = %+v, want the primary chain's aggregate check kept", body.Checks["rpc"])
	}
}
