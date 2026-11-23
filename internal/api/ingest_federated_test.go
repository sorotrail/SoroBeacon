package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// federatedIngestServer builds an API over a real SQLite store with one
// monitor, so a payload naming that monitor's id can be shown not to attach
// itself to it.
func federatedIngestServer(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	m := &store.Monitor{Name: "local", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(context.Background(), m))

	// Mount the API where a real sender expects it. The federation notifier
	// posts to <base>/api/v1/ingest, so this test exercises the same path.
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", authServerOn(st, auth.New([]string{"peer-token"}, 0))))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

// postFederated sends a federated alert naming the given monitor and rule.
func postFederated(t *testing.T, srv *httptest.Server, monitorID, ruleID int64, origin string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"alert": map[string]any{
			"monitor_id": monitorID,
			"rule_id":    ruleID,
			"event_id":   "evt-1",
			"payload":    map[string]any{"contract_id": "C1"},
		},
		"origin":    origin,
		"hop_count": 1,
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ingest", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer peer-token")
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	return res
}

// A payload that names a monitor this instance happens to have must not be
// attached to it. The ids in the payload are the sending instance's, so the
// alert belongs to no local monitor at all.
func TestIngestDoesNotAttachToALocalMonitor(t *testing.T) {
	srv, st := federatedIngestServer(t)

	// Monitor 1 is the one the fixture created.
	res := postFederated(t, srv, 1, 1, "peer-a")
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusCreated, res.StatusCode)

	alerts, err := st.ListAlerts(context.Background(), store.AlertFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, alerts, 1)
	stored := alerts[0]
	assert.True(t, stored.Federated)
	assert.Equal(t, "peer-a", stored.FederationOrigin)
	assert.Equal(t, int64(1), stored.SourceMonitorID, "the sender's monitor id is kept")
	assert.Equal(t, int64(1), stored.SourceRuleID, "the sender's rule id is kept")
	assert.Zero(t, stored.MonitorID, "the alert must not be attached to the local monitor")
	assert.Zero(t, stored.RuleID, "the alert must not be attached to a local rule")

	// The local monitor's own stats do not count the federated alert: it did
	// not fire here.
	stats, err := st.GetMonitorStats(context.Background(), 1)
	require.NoError(t, err)
	assert.Zero(t, stats.Alerts, "a federated alert is not a match of a local monitor")
}

// A payload naming ids this instance does not have is stored the same way:
// there is nothing to attach it to, and the answer is 201.
func TestIngestAcceptsUnknownIDs(t *testing.T) {
	srv, st := federatedIngestServer(t)

	res := postFederated(t, srv, 4242, 99, "peer-a")
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusCreated, res.StatusCode)

	alerts, err := st.ListAlerts(context.Background(), store.AlertFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, alerts, 1)
	assert.Equal(t, int64(4242), alerts[0].SourceMonitorID)
	assert.Zero(t, alerts[0].MonitorID)
}

// The alert lands in the workspace of the credential that delivered it, never
// in one the payload names: the payload is the other instance's data and
// cannot choose this instance's tenancy.
func TestIngestLandsInTheDeliveringWorkspace(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	a := auth.NewBound([]auth.Binding{
		{Workspace: workspace.ID("acme"), Token: "acme-token"},
	}, 0)
	srv := httptest.NewServer(authServerOn(st, a))
	defer srv.Close()

	body, err := json.Marshal(map[string]any{
		"alert":     map[string]any{"monitor_id": 1, "rule_id": 1, "event_id": "evt-1"},
		"origin":    "peer-a",
		"hop_count": 1,
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/ingest", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer acme-token")
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusCreated, res.StatusCode)

	acmeAlerts, err := st.ListAlerts(workspace.With(context.Background(), workspace.ID("acme")), store.AlertFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, acmeAlerts, 1, "the alert belongs to the delivering credential's workspace")

	other, err := st.ListAlerts(workspace.With(context.Background(), workspace.ID("beta")), store.AlertFilter{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, other, "another workspace must not see the alert")
}

// The payload keeps the federation stamps the dashboard and any other reader
// of the stored payload has seen, plus the sender's ids.
func TestIngestStampsThePayload(t *testing.T) {
	srv, st := federatedIngestServer(t)

	res := postFederated(t, srv, 7, 8, "peer-a")
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusCreated, res.StatusCode)

	alerts, err := st.ListAlerts(context.Background(), store.AlertFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, alerts, 1)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(alerts[0].Payload, &payload))
	assert.Equal(t, true, payload["_federated"])
	assert.Equal(t, "peer-a", payload["_federation_origin"])
	assert.Equal(t, float64(1), payload["_federation_hops"])
	assert.Equal(t, float64(7), payload["_federation_monitor_id"])
	assert.Equal(t, float64(8), payload["_federation_rule_id"])
}

// A severity the database's vocabulary does not contain is a malformed
// payload: the sender is told so instead of the store refusing the row.
func TestIngestRejectsAnUnknownSeverity(t *testing.T) {
	srv, _ := federatedIngestServer(t)

	body, err := json.Marshal(map[string]any{
		"alert": map[string]any{
			"monitor_id": 1,
			"rule_id":    1,
			"event_id":   "evt-1",
			"severity":   "apocalyptic",
		},
		"origin":    "peer-a",
		"hop_count": 1,
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ingest", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer peer-token")
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, res.StatusCode)

	raw, _ := io.ReadAll(res.Body)
	assert.Contains(t, string(raw), "severity")
}

// The receiver still enforces the hop limit even though nothing re-forwards
// federated alerts today.
func TestIngestRejectsAtHopLimit(t *testing.T) {
	srv, _ := federatedIngestServer(t)

	body, err := json.Marshal(map[string]any{
		"alert": map[string]any{
			"monitor_id": 1,
			"rule_id":    1,
			"event_id":   "evt-1",
		},
		"origin":    "peer-a",
		"hop_count": notify.MaxHopCount,
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ingest", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer peer-token")
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	raw, _ := io.ReadAll(res.Body)
	assert.Contains(t, string(raw), "hop limit")
}

// A federation channel built through the production factory must stamp the
// sender's origin and be accepted by the real ingest handler. This is the
// regression test for the empty-origin bug: the old factory constructor never
// set one, so every delivery was rejected with 400.
func TestFederationChannelSendsItsOriginToRealIngest(t *testing.T) {
	srv, st := federatedIngestServer(t)

	var gotStatus int
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, r)
		gotStatus = rec.Code
		for key, values := range rec.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	defer wrapped.Close()

	factory := notify.DefaultFactory().WithFederationOrigin("beacon-a")
	cfg, err := json.Marshal(map[string]string{"url": wrapped.URL, "token": "peer-token"})
	require.NoError(t, err)
	n, err := factory.New(notify.TypeFederation, cfg)
	require.NoError(t, err)

	err = n.Send(context.Background(), notify.Alert{
		ID:          42,
		MonitorID:   1,
		MonitorName: "remote",
		RuleID:      1,
		RuleType:    "event_emitted",
		EventID:     "evt-1",
		ContractID:  "C1",
		Ledger:      100,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, gotStatus)

	alerts, err := st.ListAlerts(context.Background(), store.AlertFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, alerts, 1)
	assert.True(t, alerts[0].Federated)
	assert.Equal(t, "beacon-a", alerts[0].FederationOrigin)
}
