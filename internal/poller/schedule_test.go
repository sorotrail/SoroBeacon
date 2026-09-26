package poller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// scheduled builds one contract fixture for the scheduler tests.
func scheduled(id string, p store.Priority) scheduledContract {
	return scheduledContract{ContractID: id, Priority: p}
}

// ids extracts the ordered contract ids from a plan.
func ids(watch []Watch) []string {
	out := make([]string, len(watch))
	for i, w := range watch {
		out[i] = w.ContractID
	}
	return out
}

// indexOf returns the position of id in the plan, or -1.
func indexOf(watch []Watch, id string) int {
	for i, w := range watch {
		if w.ContractID == id {
			return i
		}
	}
	return -1
}

// TestSchedulerServesHighPriorityFirst is the core requirement: under
// contention a high-priority contract is queued ahead of the low tier.
func TestSchedulerServesHighPriorityFirst(t *testing.T) {
	s := NewScheduler()
	plan := s.Order([]scheduledContract{
		scheduled("low-1", store.PriorityLow),
		scheduled("high-1", store.PriorityHigh),
		scheduled("normal-1", store.PriorityNormal),
	})

	require.Len(t, plan, 3)
	assert.Equal(t, "high-1", plan[0].ContractID, "high tier is served first")
	assert.Less(t, indexOf(plan, "high-1"), indexOf(plan, "low-1"))
}

// TestSchedulerNeverStarvesLowPriority proves the bounded wait: with a hundred
// high-priority contracts queued, the single low-priority contract still
// appears within the first round, whose size is the sum of the tier weights.
// This is the property that makes strict priority safe.
func TestSchedulerNeverStarvesLowPriority(t *testing.T) {
	const highCount = 100
	items := make([]scheduledContract, 0, highCount+1)
	for i := 0; i < highCount; i++ {
		items = append(items, scheduled(fmt.Sprintf("high-%d", i), store.PriorityHigh))
	}
	items = append(items, scheduled("low-only", store.PriorityLow))

	plan := NewScheduler().Order(items)

	require.Len(t, plan, highCount+1, "every contract must be scheduled exactly once")
	seen := map[string]bool{}
	for _, w := range plan {
		require.False(t, seen[w.ContractID], "duplicate contract %s", w.ContractID)
		seen[w.ContractID] = true
	}
	lowAt := indexOf(plan, "low-only")
	require.NotEqual(t, -1, lowAt, "the low tier must be scheduled")
	assert.LessOrEqual(t, lowAt, tierWeight(store.PriorityHigh)+tierWeight(store.PriorityNormal)+tierWeight(store.PriorityLow)-1,
		"a low-priority contract waits at most one weighted round, regardless of how many high-priority contracts exist")
}

// TestSchedulerEmptyAndSingleTier pins the edge cases: no contracts schedules
// nothing, and a watch list with one tier keeps its input order on the first
// cycle (existing deployments behave exactly as before priorities existed).
func TestSchedulerEmptyAndSingleTier(t *testing.T) {
	s := NewScheduler()
	assert.Empty(t, s.Order(nil))

	plan := s.Order([]scheduledContract{
		scheduled("a", store.PriorityNormal),
		scheduled("b", store.PriorityNormal),
		scheduled("c", store.PriorityNormal),
	})
	assert.Equal(t, []string{"a", "b", "c"}, ids(plan), "input order is preserved within a tier")
}

// TestSchedulerRotatesWithinTier proves the rotation cursor: two cycles with
// one tier must not always serve the same contract first, or the peers of
// whatever sorts first would themselves be starved.
func TestSchedulerRotatesWithinTier(t *testing.T) {
	s := NewScheduler()
	items := []scheduledContract{
		scheduled("a", store.PriorityNormal),
		scheduled("b", store.PriorityNormal),
		scheduled("c", store.PriorityNormal),
	}

	first := ids(s.Order(items))
	second := ids(s.Order(items))

	assert.Equal(t, []string{"a", "b", "c"}, first)
	assert.Equal(t, []string{"b", "c", "a"}, second, "the next cycle starts one contract later")
	assert.NotEqual(t, first[0], second[0])
}

// TestSchedulerUnsetPriorityIsNormal guards the default: a monitor created
// before priorities existed (empty value) is scheduled in the middle tier.
func TestSchedulerUnsetPriorityIsNormal(t *testing.T) {
	plan := NewScheduler().Order([]scheduledContract{
		{ContractID: "unset"},
		scheduled("high", store.PriorityHigh),
	})
	require.Len(t, plan, 2)
	assert.Equal(t, "high", plan[0].ContractID, "an unset priority ranks below high")
}

// TestPollOrdersRequestsByPriority verifies the scheduler is actually wired
// into the ingest loop: the RPC filters reach the wire high-priority first
// even when the store returns the monitors in the opposite order.
func TestPollOrdersRequestsByPriority(t *testing.T) {
	lowContract := contractID(0x11)
	highContract := contractID(0x22)
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	st.monitors = []store.Monitor{
		{ID: 1, Name: "low", ContractIDs: []string{lowContract}, Enabled: true, Priority: store.PriorityLow},
		{ID: 2, Name: "high", ContractIDs: []string{highContract}, Enabled: true, Priority: store.PriorityHigh},
	}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 1, "both contracts share the unfiltered topic group")
	assert.Equal(t, []string{highContract, lowContract}, rpc.requests[0].Filters[0].ContractIDs,
		"the high-priority contract must be ordered first in the request")
}

// TestPollHighestPriorityWinsSharedContract pins the max-rank rule: a contract
// watched by both a low- and a high-priority monitor is scheduled as high.
func TestPollHighestPriorityWinsSharedContract(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	st.monitors = []store.Monitor{
		{ID: 1, Name: "low", ContractIDs: []string{contractA}, Enabled: true, Priority: store.PriorityLow},
		{ID: 2, Name: "high", ContractIDs: []string{contractA}, Enabled: true, Priority: store.PriorityHigh},
	}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 1)
	assert.Equal(t, []string{contractA}, rpc.requests[0].Filters[0].ContractIDs)
}
