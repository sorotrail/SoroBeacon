package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func sqliteConditionalAPI(t *testing.T) (*httptest.Server, store.Store, *metrics.Metrics) {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	m := metrics.New()
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).WithMetrics(m)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, st, m
}

func seedTestAlert(t *testing.T, st store.Store, ctx context.Context, monitorName string) (*store.Monitor, *store.Rule, *store.Alert) {
	t.Helper()
	m := &store.Monitor{Name: monitorName, Network: "testnet", ContractIDs: []string{validContract}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &store.Rule{MonitorID: m.ID, Type: "event_emitted", Severity: store.SeverityWarning, Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	a := &store.Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-" + monitorName, Ledger: 100}
	outcome, err := st.CreateAlert(ctx, a)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)
	return m, r, a
}

func doConditionalReq(t *testing.T, method, url string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res, body
}

func TestConditionalAlerts_MatchNonMatchAndAbsentHeader(t *testing.T) {
	srv, st, _ := sqliteConditionalAPI(t)
	seedTestAlert(t, st, context.Background(), "mon-1")

	// 1. Absent header: returns 200 OK with ETag and Cache-Control: private, no-cache
	res, body := doConditionalReq(t, "GET", srv.URL+"/alerts", nil)
	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.NotEmpty(t, body)
	etag := res.Header.Get("ETag")
	assert.NotEmpty(t, etag, "ETag must be set on successful read")
	assert.Equal(t, "private, no-cache", res.Header.Get("Cache-Control"))

	// 2. Matching If-None-Match: returns 304 Not Modified with empty body
	res304, body304 := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{"If-None-Match": etag})
	assert.Equal(t, http.StatusNotModified, res304.StatusCode)
	assert.Empty(t, body304, "304 response must carry no body")
	assert.Equal(t, etag, res304.Header.Get("ETag"))
	assert.Equal(t, "private, no-cache", res304.Header.Get("Cache-Control"))

	// 3. Matching weak validator W/"...": returns 304 Not Modified
	weakTag := "W/" + etag
	resWeak, bodyWeak := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{"If-None-Match": weakTag})
	assert.Equal(t, http.StatusNotModified, resWeak.StatusCode)
	assert.Empty(t, bodyWeak)

	// 4. Wildcard If-None-Match "*": returns 304 Not Modified
	resStar, bodyStar := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{"If-None-Match": "*"})
	assert.Equal(t, http.StatusNotModified, resStar.StatusCode)
	assert.Empty(t, bodyStar)

	// 5. Non-matching header: returns 200 OK with full body
	resNonMatch, bodyNonMatch := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{"If-None-Match": `"mismatched-etag"`})
	assert.Equal(t, http.StatusOK, resNonMatch.StatusCode)
	assert.NotEmpty(t, bodyNonMatch)
	assert.Equal(t, etag, resNonMatch.Header.Get("ETag"))
}

func TestConditionalAlerts_NewAlertInvalidatesTag(t *testing.T) {
	srv, st, _ := sqliteConditionalAPI(t)
	m, r, _ := seedTestAlert(t, st, context.Background(), "mon-1")

	res1, _ := doConditionalReq(t, "GET", srv.URL+"/alerts", nil)
	etag1 := res1.Header.Get("ETag")
	require.NotEmpty(t, etag1)

	// Confirm 304 with etag1
	resMatch, _ := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{"If-None-Match": etag1})
	assert.Equal(t, http.StatusNotModified, resMatch.StatusCode)

	// Add a new alert to the store
	a2 := &store.Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-mon-2", Ledger: 101}
	outcome, err := st.CreateAlert(context.Background(), a2)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	// Now conditional request with etag1 must be invalidated (return 200 OK with new tag)
	resAfter, bodyAfter := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{"If-None-Match": etag1})
	assert.Equal(t, http.StatusOK, resAfter.StatusCode, "new alert must invalidate stale ETag")
	etag2 := resAfter.Header.Get("ETag")
	assert.NotEqual(t, etag1, etag2, "new alert must produce a different ETag")

	var envelope struct {
		Alerts []store.Alert `json:"alerts"`
	}
	require.NoError(t, json.Unmarshal(bodyAfter, &envelope))
	assert.Len(t, envelope.Alerts, 2)
}

func TestConditionalAlerts_FiltersDoNotCollide(t *testing.T) {
	srv, st, _ := sqliteConditionalAPI(t)
	m1, _, _ := seedTestAlert(t, st, context.Background(), "mon-1")
	m2, _, _ := seedTestAlert(t, st, context.Background(), "mon-2")

	url1 := srv.URL + "/alerts?monitor_id=" + itoa(m1.ID)
	url2 := srv.URL + "/alerts?monitor_id=" + itoa(m2.ID)

	res1, _ := doConditionalReq(t, "GET", url1, nil)
	res2, _ := doConditionalReq(t, "GET", url2, nil)
	etag1 := res1.Header.Get("ETag")
	etag2 := res2.Header.Get("ETag")

	require.NotEmpty(t, etag1)
	require.NotEmpty(t, etag2)
	assert.NotEqual(t, etag1, etag2, "different query filters must never collide on entity tag")

	// Sending etag1 to url2 must NOT return 304
	resMismatch, _ := doConditionalReq(t, "GET", url2, map[string]string{"If-None-Match": etag1})
	assert.Equal(t, http.StatusOK, resMismatch.StatusCode)

	// Different limits must not collide
	resLimit10, _ := doConditionalReq(t, "GET", srv.URL+"/alerts?limit=10", nil)
	resLimit20, _ := doConditionalReq(t, "GET", srv.URL+"/alerts?limit=20", nil)
	assert.NotEqual(t, resLimit10.Header.Get("ETag"), resLimit20.Header.Get("ETag"))

	// Different cursors must not collide
	resCur1, _ := doConditionalReq(t, "GET", srv.URL+"/alerts?cursor=1", nil)
	resCur2, _ := doConditionalReq(t, "GET", srv.URL+"/alerts?cursor=2", nil)
	assert.NotEqual(t, resCur1.Header.Get("ETag"), resCur2.Header.Get("ETag"))
}

func TestConditionalAlerts_WorkspacesDoNotCollide(t *testing.T) {
	st := newTenantStore(t)
	authn := twoWorkspaces()
	m := metrics.New()
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithAuth(authn).
		WithMetrics(m)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	// Seed alert in acme
	seedTestAlert(t, st, workspace.With(context.Background(), "acme"), "acme-mon")
	// Seed alert in beta
	seedTestAlert(t, st, workspace.With(context.Background(), "beta"), "beta-mon")

	// Query acme
	resAcme, _ := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{
		"Authorization": "Bearer " + acmeToken,
	})
	assert.Equal(t, http.StatusOK, resAcme.StatusCode)
	etagAcme := resAcme.Header.Get("ETag")
	require.NotEmpty(t, etagAcme)

	// Query beta
	resBeta, _ := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{
		"Authorization": "Bearer " + betaToken,
	})
	assert.Equal(t, http.StatusOK, resBeta.StatusCode)
	etagBeta := resBeta.Header.Get("ETag")
	require.NotEmpty(t, etagBeta)

	assert.NotEqual(t, etagAcme, etagBeta, "different workspaces must produce different ETags (anti-disclosure)")

	// Presenting acme's ETag to beta must return 200, never 304
	resCross, _ := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{
		"Authorization": "Bearer " + betaToken,
		"If-None-Match": etagAcme,
	})
	assert.Equal(t, http.StatusOK, resCross.StatusCode, "cross-workspace ETag match must be rejected")
}

func TestConditionalAlertsDaily(t *testing.T) {
	srv, st, _ := sqliteConditionalAPI(t)
	m, r, _ := seedTestAlert(t, st, context.Background(), "mon-1")

	res, body := doConditionalReq(t, "GET", srv.URL+"/stats/alerts-daily", nil)
	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.NotEmpty(t, body)
	etag := res.Header.Get("ETag")
	assert.NotEmpty(t, etag)
	assert.Equal(t, "private, no-cache", res.Header.Get("Cache-Control"))

	// Revalidate with matching If-None-Match -> 304
	res304, body304 := doConditionalReq(t, "GET", srv.URL+"/stats/alerts-daily", map[string]string{"If-None-Match": etag})
	assert.Equal(t, http.StatusNotModified, res304.StatusCode)
	assert.Empty(t, body304)

	// New alert invalidates tag
	a2 := &store.Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-mon-2", Ledger: 101}
	outcome, err := st.CreateAlert(context.Background(), a2)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	resAfter, _ := doConditionalReq(t, "GET", srv.URL+"/stats/alerts-daily", map[string]string{"If-None-Match": etag})
	assert.Equal(t, http.StatusOK, resAfter.StatusCode)
	assert.NotEqual(t, etag, resAfter.Header.Get("ETag"))
}

func TestConditionalChannelDeliveryStats(t *testing.T) {
	srv, st, _ := sqliteConditionalAPI(t)
	ctx := context.Background()
	_, _, a := seedTestAlert(t, st, ctx, "mon-1")

	ch := &store.Channel{Name: "slack", Type: "webhook", Enabled: true, Config: json.RawMessage(`{"url":"https://example.com"}`)}
	require.NoError(t, st.CreateChannel(ctx, ch))

	url := srv.URL + "/channels/" + itoa(ch.ID) + "/stats"

	// 1. Initial read
	res1, _ := doConditionalReq(t, "GET", url, nil)
	assert.Equal(t, http.StatusOK, res1.StatusCode)
	etag1 := res1.Header.Get("ETag")
	require.NotEmpty(t, etag1)
	assert.Equal(t, "private, no-cache", res1.Header.Get("Cache-Control"))

	// 2. Revalidate matching If-None-Match -> 304
	res304, body304 := doConditionalReq(t, "GET", url, map[string]string{"If-None-Match": etag1})
	assert.Equal(t, http.StatusNotModified, res304.StatusCode)
	assert.Empty(t, body304)

	// 3. New delivery attempt invalidates the ETag
	da := &store.DeliveryAttempt{AlertID: a.ID, ChannelID: ch.ID, Status: "success", AttemptedAt: time.Now().UTC()}
	require.NoError(t, st.RecordDeliveryAttempt(ctx, da))

	resAfter, _ := doConditionalReq(t, "GET", url, map[string]string{"If-None-Match": etag1})
	assert.Equal(t, http.StatusOK, resAfter.StatusCode, "new delivery attempt must invalidate channel stats ETag")
	etag2 := resAfter.Header.Get("ETag")
	assert.NotEqual(t, etag1, etag2)

	// 4. Different window param produces different ETag
	res90m, _ := doConditionalReq(t, "GET", url+"?window=90m", nil)
	assert.NotEqual(t, etag2, res90m.Header.Get("ETag"))

	// 5. Unknown channel returns 404
	res404, _ := doConditionalReq(t, "GET", srv.URL+"/channels/999999/stats", map[string]string{"If-None-Match": etag2})
	assert.Equal(t, http.StatusNotFound, res404.StatusCode)
}

// countingStore intercepts ListAlerts to prove that a 304 Not Modified skips the query.
type queryCountingStore struct {
	store.Store
	listAlertsCount int64
}

func (c *queryCountingStore) ListAlerts(ctx context.Context, f store.AlertFilter) ([]store.Alert, error) {
	atomic.AddInt64(&c.listAlertsCount, 1)
	return c.Store.ListAlerts(ctx, f)
}

func TestConditionalAlerts_QuerySkippedOnNotModified(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, store.Migrate(url))
	realSt, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(realSt.Close)

	cs := &queryCountingStore{Store: realSt}
	seedTestAlert(t, realSt, context.Background(), "mon-1")

	s := New(cs, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	// First call: full GET (status 200). Must invoke ListAlerts once.
	res1, _ := doConditionalReq(t, "GET", srv.URL+"/alerts", nil)
	assert.Equal(t, http.StatusOK, res1.StatusCode)
	etag := res1.Header.Get("ETag")
	require.NotEmpty(t, etag)
	assert.Equal(t, int64(1), atomic.LoadInt64(&cs.listAlertsCount))

	// Second call: conditional GET (status 304). Must NOT invoke ListAlerts!
	res2, _ := doConditionalReq(t, "GET", srv.URL+"/alerts", map[string]string{"If-None-Match": etag})
	assert.Equal(t, http.StatusNotModified, res2.StatusCode)
	assert.Equal(t, int64(1), atomic.LoadInt64(&cs.listAlertsCount), "ListAlerts must NOT be called on a 304 match")
}
