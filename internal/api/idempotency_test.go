package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func newIdempotencyTestServer(t *testing.T) (*Server, *store.SQLite) {
	t.Helper()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "idempotency.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), nil, log)
	return srv, st
}

func TestIdempotencyRepeatPostMonitorAndChannel(t *testing.T) {
	srv, st := newIdempotencyTestServer(t)
	router := srv.Routes()

	monitorBody := `{"name":"USDC Vault","contract_ids":["CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"]}`
	req1 := httptest.NewRequest(http.MethodPost, "/monitors", strings.NewReader(monitorBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set(IdempotencyKeyHeader, "mon-create-1")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
	assert.Empty(t, rec1.Header().Get(IdempotencyReplayedHeader))

	req2 := httptest.NewRequest(http.MethodPost, "/monitors", strings.NewReader(monitorBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set(IdempotencyKeyHeader, "mon-create-1")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusCreated, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get(IdempotencyReplayedHeader))
	assert.JSONEq(t, rec1.Body.String(), rec2.Body.String())

	monitors, err := st.ListMonitors(context.Background(), false)
	require.NoError(t, err)
	assert.Len(t, monitors, 1, "replayed POST /monitors must not create a second monitor")

	audits, err := st.ListAuditEntries(context.Background(), store.AuditFilter{TargetType: "monitor"})
	require.NoError(t, err)
	assert.Len(t, audits, 1, "replayed POST /monitors must not append a duplicate audit entry")
}

func TestIdempotencyRepeatDeleteReturns204InsteadOf404(t *testing.T) {
	srv, _ := newIdempotencyTestServer(t)
	router := srv.Routes()

	// Create a channel first.
	chBody := `{"name":"ops-slack","type":"webhook","config":{"url":"https://example.com/hook","secret":"s3cr3t"}}`
	createReq := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(chBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	delPath := "/channels/" + strconv.FormatInt(created.ID, 10)

	// First DELETE with Idempotency-Key deletes the channel and returns 204.
	delReq1 := httptest.NewRequest(http.MethodDelete, delPath, nil)
	delReq1.Header.Set(IdempotencyKeyHeader, "del-channel-1")
	delRec1 := httptest.NewRecorder()
	router.ServeHTTP(delRec1, delReq1)
	require.Equal(t, http.StatusNoContent, delRec1.Code)
	assert.Empty(t, delRec1.Header().Get(IdempotencyReplayedHeader))

	// Second DELETE with the same Idempotency-Key replays 204 instead of 404.
	delReq2 := httptest.NewRequest(http.MethodDelete, delPath, nil)
	delReq2.Header.Set(IdempotencyKeyHeader, "del-channel-1")
	delRec2 := httptest.NewRecorder()
	router.ServeHTTP(delRec2, delReq2)
	require.Equal(t, http.StatusNoContent, delRec2.Code)
	assert.Equal(t, "true", delRec2.Header().Get(IdempotencyReplayedHeader))

	// A DELETE with a different Idempotency-Key executes against the store and sees 404.
	delReq3 := httptest.NewRequest(http.MethodDelete, delPath, nil)
	delReq3.Header.Set(IdempotencyKeyHeader, "del-channel-different-key")
	delRec3 := httptest.NewRecorder()
	router.ServeHTTP(delRec3, delReq3)
	require.Equal(t, http.StatusNotFound, delRec3.Code)
}

func TestIdempotencyBodyMethodAndPathMismatchReturn409Conflict(t *testing.T) {
	srv, _ := newIdempotencyTestServer(t)
	router := srv.Routes()

	bodyA := `{"name":"channel-a","type":"webhook","config":{"url":"https://example.com/a","secret":"s3cr3t"}}`
	bodyB := `{"name":"channel-b","type":"webhook","config":{"url":"https://example.com/b","secret":"s3cr3t"}}`

	req1 := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(bodyA))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set(IdempotencyKeyHeader, "mismatch-key")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	// Same key, different body -> 409 Conflict.
	reqDiffBody := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(bodyB))
	reqDiffBody.Header.Set("Content-Type", "application/json")
	reqDiffBody.Header.Set(IdempotencyKeyHeader, "mismatch-key")
	recDiffBody := httptest.NewRecorder()
	router.ServeHTTP(recDiffBody, reqDiffBody)
	require.Equal(t, http.StatusConflict, recDiffBody.Code)
	var env map[string]any
	require.NoError(t, json.Unmarshal(recDiffBody.Body.Bytes(), &env))
	assert.Equal(t, apierr.CodeConflict, env["code"])

	// Same key, different path -> 409 Conflict.
	reqDiffPath := httptest.NewRequest(http.MethodPost, "/monitors", strings.NewReader(bodyA))
	reqDiffPath.Header.Set("Content-Type", "application/json")
	reqDiffPath.Header.Set(IdempotencyKeyHeader, "mismatch-key")
	recDiffPath := httptest.NewRecorder()
	router.ServeHTTP(recDiffPath, reqDiffPath)
	require.Equal(t, http.StatusConflict, recDiffPath.Code)
}

func TestIdempotencyConcurrentDuplicatesCreateSingleResource(t *testing.T) {
	srv, st := newIdempotencyTestServer(t)
	router := srv.Routes()

	const workers = 10
	payload := []byte(`{"name":"concurrent-hook","type":"webhook","config":{"url":"https://example.com/concurrent","secret":"s3cr3t"}}`)

	type result struct {
		status int
		id     int64
		body   string
	}
	results := make([]result, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := range workers {
		go func(idx int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/channels", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(IdempotencyKeyHeader, "concurrent-post-key")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			var parsed struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
			results[idx] = result{
				status: rec.Code,
				id:     parsed.ID,
				body:   rec.Body.String(),
			}
		}(i)
	}
	close(start)
	wg.Wait()

	var expectedID int64
	for i, r := range results {
		require.Equal(t, http.StatusCreated, r.status, "worker %d body: %s", i, r.body)
		require.NotZero(t, r.id)
		if i == 0 {
			expectedID = r.id
		} else {
			assert.Equal(t, expectedID, r.id, "all concurrent callers must receive the same created resource ID")
		}
	}

	channels, err := st.ListChannels(context.Background(), false)
	require.NoError(t, err)
	assert.Len(t, channels, 1, "concurrent duplicate POSTs must create exactly one channel")
}

func TestIdempotencyKeyExpiryAllowsReuse(t *testing.T) {
	srv, st := newIdempotencyTestServer(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	srv.WithIdempotencyTTL(5 * time.Minute).WithIdempotencyClock(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	})
	router := srv.Routes()

	body1 := `{"name":"before-expiry","type":"webhook","config":{"url":"https://example.com/1","secret":"s3cr3t"}}`
	req1 := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set(IdempotencyKeyHeader, "ttl-key")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	// Advance clock past the 5-minute TTL.
	clockMu.Lock()
	now = now.Add(6 * time.Minute)
	clockMu.Unlock()

	body2 := `{"name":"after-expiry","type":"webhook","config":{"url":"https://example.com/2","secret":"s3cr3t"}}`
	req2 := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set(IdempotencyKeyHeader, "ttl-key")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusCreated, rec2.Code)
	assert.Empty(t, rec2.Header().Get(IdempotencyReplayedHeader))

	channels, err := st.ListChannels(context.Background(), false)
	require.NoError(t, err)
	assert.Len(t, channels, 2)
}

func TestIdempotencyCrossWorkspaceIsolation(t *testing.T) {
	srv, _ := newIdempotencyTestServer(t)
	router := srv.Routes()

	bodyA := `{"name":"ws-a-chan","type":"webhook","config":{"url":"https://example.com/a","secret":"s3cr3t"}}`
	reqA := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(bodyA))
	reqA = reqA.WithContext(workspace.With(reqA.Context(), "ws-a"))
	reqA.Header.Set("Content-Type", "application/json")
	reqA.Header.Set(IdempotencyKeyHeader, "shared-ws-key")
	recA := httptest.NewRecorder()
	router.ServeHTTP(recA, reqA)
	require.Equal(t, http.StatusCreated, recA.Code)

	bodyB := `{"name":"ws-b-chan","type":"webhook","config":{"url":"https://example.com/b","secret":"s3cr3t"}}`
	reqB := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(bodyB))
	reqB = reqB.WithContext(workspace.With(reqB.Context(), "ws-b"))
	reqB.Header.Set("Content-Type", "application/json")
	reqB.Header.Set(IdempotencyKeyHeader, "shared-ws-key")
	recB := httptest.NewRecorder()
	router.ServeHTTP(recB, reqB)
	require.Equal(t, http.StatusCreated, recB.Code)
	assert.Empty(t, recB.Header().Get(IdempotencyReplayedHeader))
}

func TestIdempotencyValidationAndClientErrorReplay(t *testing.T) {
	srv, _ := newIdempotencyTestServer(t)
	router := srv.Routes()

	// Overlong Idempotency-Key (> 255 chars) -> 400 INVALID_ARGUMENT.
	longReq := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(`{}`))
	longReq.Header.Set(IdempotencyKeyHeader, strings.Repeat("k", store.MaxIdempotencyKeyLength+1))
	longRec := httptest.NewRecorder()
	router.ServeHTTP(longRec, longReq)
	require.Equal(t, http.StatusBadRequest, longRec.Code)

	// Client validation error (400) is stored and replayed on retry.
	badBody := `{"name":"","type":"webhook","config":{"url":"https://example.com"}}`
	req1 := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(badBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set(IdempotencyKeyHeader, "bad-req-key")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusBadRequest, rec1.Code)

	req2 := httptest.NewRequest(http.MethodPost, "/channels", strings.NewReader(badBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set(IdempotencyKeyHeader, "bad-req-key")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusBadRequest, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get(IdempotencyReplayedHeader))
	assert.JSONEq(t, rec1.Body.String(), rec2.Body.String())
}
