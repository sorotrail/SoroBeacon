package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestIdempotencyStoreLifecycleAndReplay(t *testing.T) {
	st := newTestSQLite(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	exp := now.Add(DefaultIdempotencyTTL)
	hash := HashIdempotentRequest("POST", "/channels", []byte(`{"name":"ops","type":"webhook","config":{"url":"https://example.com"}}`))

	outcome, rec, err := st.ClaimIdempotencyKey(ctx, "key-1", "POST", "/channels", hash, now, exp)
	require.NoError(t, err)
	assert.Equal(t, IdempotencyClaimed, outcome)
	require.NotNil(t, rec)
	assert.False(t, rec.Completed)
	assert.Equal(t, hash, rec.RequestHash)

	// While in-progress, a duplicate request with identical parameters sees IdempotencyInProgress.
	outcome, inProg, err := st.ClaimIdempotencyKey(ctx, "key-1", "POST", "/channels", hash, now.Add(time.Second), exp)
	require.NoError(t, err)
	assert.Equal(t, IdempotencyInProgress, outcome)
	require.NotNil(t, inProg)
	assert.False(t, inProg.Completed)

	// Completing the claim stores the status, content-type, and response body.
	respBody := []byte(`{"id":1,"name":"ops"}`)
	require.NoError(t, st.CompleteIdempotencyKey(ctx, "key-1", hash, 201, "application/json", respBody, exp))

	// Subsequent identical request replays the stored response.
	outcome, replayed, err := st.ClaimIdempotencyKey(ctx, "key-1", "POST", "/channels", hash, now.Add(2*time.Second), exp)
	require.NoError(t, err)
	assert.Equal(t, IdempotencyReplayed, outcome)
	require.NotNil(t, replayed)
	assert.True(t, replayed.Completed)
	assert.Equal(t, 201, replayed.StatusCode)
	assert.Equal(t, "application/json", replayed.ContentType)
	assert.Equal(t, respBody, replayed.ResponseBody)

	// GetIdempotencyKey returns the stored record before expiry.
	fetched, err := st.GetIdempotencyKey(ctx, "key-1", now.Add(3*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 201, fetched.StatusCode)
	assert.Equal(t, respBody, fetched.ResponseBody)
}

func TestIdempotencyStoreConflictOnBodyMethodOrPathMismatch(t *testing.T) {
	st := newTestSQLite(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	exp := now.Add(DefaultIdempotencyTTL)
	hashA := HashIdempotentRequest("POST", "/channels", []byte(`{"name":"alpha"}`))
	hashB := HashIdempotentRequest("POST", "/channels", []byte(`{"name":"beta"}`))

	outcome, _, err := st.ClaimIdempotencyKey(ctx, "conflict-key", "POST", "/channels", hashA, now, exp)
	require.NoError(t, err)
	assert.Equal(t, IdempotencyClaimed, outcome)
	require.NoError(t, st.CompleteIdempotencyKey(ctx, "conflict-key", hashA, 201, "application/json", []byte(`{"id":1}`), exp))

	// Different body hash -> ErrIdempotencyKeyConflict.
	outcome, _, err = st.ClaimIdempotencyKey(ctx, "conflict-key", "POST", "/channels", hashB, now.Add(time.Second), exp)
	assert.ErrorIs(t, err, ErrIdempotencyKeyConflict)
	assert.Equal(t, IdempotencyConflict, outcome)

	// Different path -> ErrIdempotencyKeyConflict.
	outcome, _, err = st.ClaimIdempotencyKey(ctx, "conflict-key", "POST", "/monitors", hashA, now.Add(time.Second), exp)
	assert.ErrorIs(t, err, ErrIdempotencyKeyConflict)
	assert.Equal(t, IdempotencyConflict, outcome)

	// Different method -> ErrIdempotencyKeyConflict.
	outcome, _, err = st.ClaimIdempotencyKey(ctx, "conflict-key", "PATCH", "/channels", hashA, now.Add(time.Second), exp)
	assert.ErrorIs(t, err, ErrIdempotencyKeyConflict)
	assert.Equal(t, IdempotencyConflict, outcome)
}

func TestIdempotencyStoreExpiryAndPruning(t *testing.T) {
	st := newTestSQLite(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ttl := 10 * time.Minute
	exp := now.Add(ttl)
	hashA := HashIdempotentRequest("POST", "/channels", []byte(`{"name":"first"}`))
	hashB := HashIdempotentRequest("POST", "/channels", []byte(`{"name":"second"}`))

	outcome, _, err := st.ClaimIdempotencyKey(ctx, "expiring-key", "POST", "/channels", hashA, now, exp)
	require.NoError(t, err)
	assert.Equal(t, IdempotencyClaimed, outcome)
	require.NoError(t, st.CompleteIdempotencyKey(ctx, "expiring-key", hashA, 201, "application/json", []byte(`{"id":1}`), exp))

	// After expiry, GetIdempotencyKey returns ErrNotFound.
	_, err = st.GetIdempotencyKey(ctx, "expiring-key", exp.Add(time.Second))
	assert.ErrorIs(t, err, ErrNotFound)

	// Claiming after expiry succeeds even with a different payload.
	afterExp := exp.Add(time.Second)
	outcome, rec, err := st.ClaimIdempotencyKey(ctx, "expiring-key", "POST", "/channels", hashB, afterExp, afterExp.Add(ttl))
	require.NoError(t, err)
	assert.Equal(t, IdempotencyClaimed, outcome)
	assert.Equal(t, hashB, rec.RequestHash)

	// PruneExpiredIdempotencyKeys removes expired rows.
	pruned, err := st.PruneExpiredIdempotencyKeys(ctx, afterExp.Add(ttl+time.Second))
	require.NoError(t, err)
	assert.Equal(t, int64(1), pruned)
}

func TestIdempotencyStoreWorkspaceIsolation(t *testing.T) {
	st := newTestSQLite(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	exp := now.Add(DefaultIdempotencyTTL)

	ctxA := workspace.With(context.Background(), "ws-alpha")
	ctxB := workspace.With(context.Background(), "ws-beta")

	hashA := HashIdempotentRequest("POST", "/channels", []byte(`{"name":"alpha"}`))
	hashB := HashIdempotentRequest("POST", "/channels", []byte(`{"name":"beta"}`))

	outcomeA, _, err := st.ClaimIdempotencyKey(ctxA, "shared-key", "POST", "/channels", hashA, now, exp)
	require.NoError(t, err)
	assert.Equal(t, IdempotencyClaimed, outcomeA)

	outcomeB, _, err := st.ClaimIdempotencyKey(ctxB, "shared-key", "POST", "/channels", hashB, now, exp)
	require.NoError(t, err)
	assert.Equal(t, IdempotencyClaimed, outcomeB)
}

func TestIdempotencyStoreConcurrentClaims(t *testing.T) {
	st := newTestSQLite(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	exp := now.Add(DefaultIdempotencyTTL)
	hash := HashIdempotentRequest("POST", "/monitors", []byte(`{"name":"m1","contract_id":"CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"}`))

	const workers = 10
	var claimed, inProgress atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			outcome, _, err := st.ClaimIdempotencyKey(ctx, "concurrent-key", "POST", "/monitors", hash, now, exp)
			require.NoError(t, err)
			switch outcome {
			case IdempotencyClaimed:
				claimed.Add(1)
			case IdempotencyInProgress:
				inProgress.Add(1)
			default:
				t.Errorf("unexpected outcome: %s", outcome)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), claimed.Load())
	assert.Equal(t, int32(workers-1), inProgress.Load())
}

type embeddedNilIdempotencyStore struct {
	Store
}

func TestSafeIdempotencyHelpersTolerateEmbeddedNilStore(t *testing.T) {
	var fake embeddedNilIdempotencyStore
	now := time.Now().UTC()
	outcome, rec, supported, err := SafeClaimIdempotencyKey(context.Background(), fake, "k", "POST", "/channels", "h", now, now.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, supported)
	assert.Empty(t, outcome)
	assert.Nil(t, rec)

	require.NoError(t, SafeCompleteIdempotencyKey(context.Background(), fake, "k", "h", 200, "application/json", nil, now.Add(time.Hour)))
	_, err = SafeGetIdempotencyKey(context.Background(), fake, "k", now)
	assert.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, SafeDeleteIdempotencyKey(context.Background(), fake, "k"))
}
