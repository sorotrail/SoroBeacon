package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func newQuotaTestSQLite(t *testing.T) *SQLite {
	t.Helper()
	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "quotas.db")
	require.NoError(t, Migrate(dbURL))
	st, err := New(context.Background(), dbURL, PoolSettings{}, nil)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	sq, ok := st.(*SQLite)
	require.True(t, ok)
	return sq
}

func TestWorkspaceQuotas_ResourceEnforcementAndIsolation(t *testing.T) {
	st := newQuotaTestSQLite(t)
	sys := workspace.WithSystem(context.Background())
	wsA := workspace.With(context.Background(), "alpha")
	wsB := workspace.With(context.Background(), "beta")

	_, err := st.SetWorkspaceQuota(sys, WorkspaceQuota{
		WorkspaceID:  "alpha",
		MaxMonitors:  2,
		MaxRules:     3,
		MaxChannels:  1,
	})
	require.NoError(t, err)

	// Monitor quota (limit = 2)
	m1 := &Monitor{Name: "m1", ContractIDs: []string{"C1"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(wsA, m1))
	m2 := &Monitor{Name: "m2", ContractIDs: []string{"C2"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(wsA, m2))

	m3 := &Monitor{Name: "m3", ContractIDs: []string{"C3"}, Enabled: true}
	err = st.CreateMonitor(wsA, m3)
	var qErr *QuotaExceededError
	require.ErrorAs(t, err, &qErr)
	require.ErrorIs(t, err, ErrQuotaExceeded)
	assert.Equal(t, "alpha", qErr.WorkspaceID)
	assert.Equal(t, QuotaResourceMonitors, qErr.Resource)
	assert.Equal(t, int64(2), qErr.Limit)
	assert.Equal(t, int64(2), qErr.Current)

	// Soft-deleting m2 frees a slot, allowing m3; restoring m2 then hits the quota!
	require.NoError(t, st.SoftDeleteMonitor(wsA, m2.ID))
	require.NoError(t, st.CreateMonitor(wsA, m3))
	err = st.RestoreMonitor(wsA, m2.ID)
	require.ErrorIs(t, err, ErrQuotaExceeded)

	// Rule quota (limit = 3 across active monitors in workspace alpha)
	for i := 0; i < 3; i++ {
		r := &Rule{MonitorID: m1.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
		require.NoError(t, st.CreateRule(wsA, r))
	}
	r4 := &Rule{MonitorID: m3.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	err = st.CreateRule(wsA, r4)
	require.ErrorAs(t, err, &qErr)
	assert.Equal(t, QuotaResourceRules, qErr.Resource)
	assert.Equal(t, int64(3), qErr.Limit)
	assert.Equal(t, int64(3), qErr.Current)

	// Channel quota (limit = 1)
	c1 := &Channel{Name: "c1", Type: "webhook", Config: json.RawMessage(`{"url":"https://example.test"}`), Enabled: true}
	require.NoError(t, st.CreateChannel(wsA, c1))
	c2 := &Channel{Name: "c2", Type: "webhook", Config: json.RawMessage(`{"url":"https://example.test"}`), Enabled: true}
	err = st.CreateChannel(wsA, c2)
	require.ErrorAs(t, err, &qErr)
	assert.Equal(t, QuotaResourceChannels, qErr.Resource)
	assert.Equal(t, int64(1), qErr.Limit)
	assert.Equal(t, int64(1), qErr.Current)

	// Workspace beta has no quota configured and is completely unaffected.
	for i := 0; i < 4; i++ {
		mb := &Monitor{Name: fmt.Sprintf("beta-%d", i), ContractIDs: []string{"CB"}, Enabled: true}
		require.NoError(t, st.CreateMonitor(wsB, mb))
	}
}

func TestWorkspaceQuotas_ConcurrentRequestsAtBoundary(t *testing.T) {
	st := newQuotaTestSQLite(t)
	sys := workspace.WithSystem(context.Background())
	ws := workspace.With(context.Background(), "concurrent-ws")

	_, err := st.SetWorkspaceQuota(sys, WorkspaceQuota{
		WorkspaceID: "concurrent-ws",
		MaxMonitors: 3,
	})
	require.NoError(t, err)

	const workers = 10
	var successes atomic.Int32
	var quotaRejections atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			m := &Monitor{
				Name:        fmt.Sprintf("concurrent-mon-%d", idx),
				ContractIDs: []string{"C1"},
				Enabled:     true,
			}
			err := st.CreateMonitor(ws, m)
			if err == nil {
				successes.Add(1)
			} else if assert.ErrorIs(t, err, ErrQuotaExceeded) {
				quotaRejections.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(3), successes.Load(), "exactly MaxMonitors creations must succeed")
	assert.Equal(t, int32(workers-3), quotaRejections.Load(), "remaining concurrent creations must fail with ErrQuotaExceeded")
}

func TestWorkspaceQueryBudget_ConsumeRefillAndReconcile(t *testing.T) {
	st := newQuotaTestSQLite(t)
	sys := workspace.WithSystem(context.Background())

	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	_, err := st.SetWorkspaceQuota(sys, WorkspaceQuota{
		WorkspaceID:          "budget-ws",
		QueryCostBudget:      100,
		QueryCostRefillPerSec: 10, // 10 tokens/sec
	})
	require.NoError(t, err)

	// 1. Consume 60 tokens at t0 -> allowed, 40 remaining
	dec, err := st.ConsumeWorkspaceQueryBudget(sys, "budget-ws", 60, t0)
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.InDelta(t, 40, dec.Remaining, 0.01)

	// 2. Reconcile actual cost was 45 instead of 60 (delta = -15) -> refunds 15 tokens -> 55 remaining
	rem, err := st.ReconcileWorkspaceQueryBudget(sys, "budget-ws", -15, t0)
	require.NoError(t, err)
	assert.InDelta(t, 55, rem, 0.01)

	// 3. Attempt to consume 70 tokens at t0 -> rejected (need 15 more at 10/sec -> RetryAfter = 2s)
	dec, err = st.ConsumeWorkspaceQueryBudget(sys, "budget-ws", 70, t0)
	require.NoError(t, err)
	assert.False(t, dec.Allowed)
	assert.InDelta(t, 55, dec.Remaining, 0.01)
	assert.Equal(t, 2*time.Second, dec.RetryAfter)

	// 4. Advance clock by 2 seconds (+20 tokens -> 75 available) -> consuming 70 succeeds with 5 remaining!
	t1 := t0.Add(2 * time.Second)
	dec, err = st.ConsumeWorkspaceQueryBudget(sys, "budget-ws", 70, t1)
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.InDelta(t, 5, dec.Remaining, 0.01)
}
