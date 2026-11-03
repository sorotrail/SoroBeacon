package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type degradedMockStore struct {
	store.Store
	degraded   atomic.Bool
	queryCount atomic.Int64
}

func (m *degradedMockStore) Degraded() bool {
	return m.degraded.Load()
}

func (m *degradedMockStore) Ping(ctx context.Context) error {
	m.queryCount.Add(1)
	if m.degraded.Load() {
		return store.ErrUnavailable
	}
	return nil
}

func (m *degradedMockStore) ListMonitorsPage(ctx context.Context, f store.ListFilter) ([]store.Monitor, error) {
	m.queryCount.Add(1)
	if m.degraded.Load() {
		return nil, store.ErrUnavailable
	}
	return []store.Monitor{}, nil
}

func (m *degradedMockStore) ListMonitors(ctx context.Context, enabledOnly bool) ([]store.Monitor, error) {
	m.queryCount.Add(1)
	if m.degraded.Load() {
		return nil, store.ErrUnavailable
	}
	return []store.Monitor{}, nil
}

func (m *degradedMockStore) ListAlerts(ctx context.Context, filter store.AlertFilter) ([]store.Alert, error) {
	m.queryCount.Add(1)
	if m.degraded.Load() {
		return nil, store.ErrUnavailable
	}
	return []store.Alert{}, nil
}

func (m *degradedMockStore) ListChannels(ctx context.Context, enabledOnly bool) ([]store.Channel, error) {
	m.queryCount.Add(1)
	if m.degraded.Load() {
		return nil, store.ErrUnavailable
	}
	return []store.Channel{}, nil
}

func (m *degradedMockStore) GetStats(ctx context.Context) (store.Stats, error) {
	m.queryCount.Add(1)
	if m.degraded.Load() {
		return store.Stats{}, store.ErrUnavailable
	}
	return store.Stats{}, nil
}

func (m *degradedMockStore) CreateMonitor(ctx context.Context, mon *store.Monitor) error {
	m.queryCount.Add(1)
	if m.degraded.Load() {
		return store.ErrUnavailable
	}
	return nil
}

func TestDegradedMode_FastFailureAndNoStoreQueries(t *testing.T) {
	st := &degradedMockStore{}
	st.degraded.Store(true)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	m := metrics.New()

	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, logger).
		WithMetrics(m)
	handler := srv.Routes()

	endpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/monitors", ""},
		{http.MethodGet, "/alerts", ""},
		{http.MethodGet, "/channels", ""},
		{http.MethodGet, "/stats", ""},
		{http.MethodPost, "/monitors", `{"name":"test"}`},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			var bodyReader io.Reader
			if ep.body != "" {
				bodyReader = strings.NewReader(ep.body)
			}
			req := httptest.NewRequest(ep.method, ep.path, bodyReader)
			if ep.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code, "must return 503 Service Unavailable")
			assert.Equal(t, "5", rec.Header().Get("Retry-After"), "must set Retry-After header")

			var env struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, apierr.CodeServiceUnavailable, env.Code)
		})
	}

	// CRITICAL REQUIREMENT: Assert that a declared degraded-mode endpoint issues NO query to the store!
	assert.Equal(t, int64(0), st.queryCount.Load(), "declared degraded-mode endpoints must issue zero queries to the store")

	// Verify log volume: degraded requests must not produce an "api error" ERROR log for every request.
	assert.NotContains(t, logBuf.String(), `"level":"ERROR"`, "degraded requests should not produce individual error log lines")
}

func TestDegradedMode_ExemptProbesAndReadinessFlipping(t *testing.T) {
	st := &degradedMockStore{}
	st.degraded.Store(true)

	m := metrics.New()
	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).
		WithMetrics(m)
	handler := srv.Routes()

	// 1. livez must always answer 200 even while database is degraded so orchestrator does not restart replica
	reqLive := httptest.NewRequest(http.MethodGet, "/livez", nil)
	recLive := httptest.NewRecorder()
	handler.ServeHTTP(recLive, reqLive)
	require.Equal(t, http.StatusOK, recLive.Code)
	assert.Contains(t, recLive.Body.String(), `"status":"alive"`)

	// 2. version endpoint answers 200
	reqVer := httptest.NewRequest(http.MethodGet, "/version", nil)
	recVer := httptest.NewRecorder()
	handler.ServeHTTP(recVer, reqVer)
	require.Equal(t, http.StatusOK, recVer.Code)

	// 3. readyz reports 503 and not ready so load balancer routes traffic away
	reqReady := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recReady := httptest.NewRecorder()
	handler.ServeHTTP(recReady, reqReady)
	require.Equal(t, http.StatusServiceUnavailable, recReady.Code)
	assert.Contains(t, recReady.Body.String(), `"status":"not ready"`)
	assert.Contains(t, recReady.Body.String(), `"database"`)

	// 4. Recovery: database comes back online
	st.degraded.Store(false)
	srv.breaker.RecordSuccess()

	// Readyz recovers
	reqReady2 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recReady2 := httptest.NewRecorder()
	handler.ServeHTTP(recReady2, reqReady2)
	require.Equal(t, http.StatusOK, recReady2.Code)
	assert.Contains(t, recReady2.Body.String(), `"status":"ready"`)

	// Normal endpoints answer again
	reqMon := httptest.NewRequest(http.MethodGet, "/monitors", nil)
	recMon := httptest.NewRecorder()
	handler.ServeHTTP(recMon, reqMon)
	require.Equal(t, http.StatusOK, recMon.Code)
}
