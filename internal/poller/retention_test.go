package poller

// Coverage for the retention-gap detection on resume (issue: a checkpoint
// that has fallen outside the source's retention window). Both halves of the
// failure are invisible without this: the operator sees either an erroring
// poller or — worse — a working service with a hole nobody recorded. These
// tests pin the contract: jump to the oldest retained ledger, persist the
// gap, and leave a normal resume inside the window byte-for-byte unchanged.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResumeBehindRetentionJumpsAndRecordsGap is the core scenario: the
// service was down past the node's ~1-7 day retention window, so the saved
// checkpoint names ledgers getEvents has forgotten. The poller must recover
// automatically instead of erroring in a loop, and the hole must be
// persisted — a record that outlives the process, not a log line.
func TestResumeBehindRetentionJumpsAndRecordsGap(t *testing.T) {
	ctx := context.Background()
	// The node only holds ledgers 6000+; the checkpoint stopped at 5500.
	rpc := &fakeRPC{latest: 6100, oldest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newResumePoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(ctx))

	// Automatic recovery: the first fetch starts at the oldest retained
	// ledger rather than requesting a range the node cannot serve.
	require.Len(t, rpc.requests, 1)
	assert.Equal(t, uint32(6000), rpc.requests[0].StartLedger,
		"polling must jump to the oldest retained ledger, not hammer forgotten ones")

	// The gap is recorded: start ledger, end ledger, and it went to the
	// store rather than only the log.
	require.Len(t, st.retentionGaps, 1)
	gap := st.retentionGaps[0]
	assert.Equal(t, uint32(5501), gap.FromLedger, "the gap starts just after the stale checkpoint")
	assert.Equal(t, uint32(5999), gap.ToLedger, "the gap ends just before the oldest retained ledger")

	// The checkpoint moved forward with the jump and then closed normally.
	assert.GreaterOrEqual(t, st.state.LastLedger, uint32(6000),
		"the checkpoint must have advanced past the hole")

	// A restart over the same store finds the record still there and
	// detects no new gap: the resume is inside the window now.
	resumed := newResumePoller(rpc, st, &fakeDispatcher{})
	require.NoError(t, resumed.Poll(ctx))
	require.Len(t, st.retentionGaps, 1,
		"a restart must not record a second gap for a hole that was already reported")
}

// TestResumeInsideRetentionRecordsNoGap pins the negative case: a normal
// resume inside the retention window records no gap and behaves exactly as
// it did before the feature existed — same fetch start, no store writes.
func TestResumeInsideRetentionRecordsNoGap(t *testing.T) {
	rpc := &fakeRPC{latest: 6100, oldest: 5000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newResumePoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Empty(t, st.retentionGaps, "a resume inside the window records no gap")
	require.Len(t, rpc.requests, 1)
	assert.Equal(t, uint32(5501), rpc.requests[0].StartLedger,
		"polling resumes just after the checkpoint, exactly as before")
}

// TestRetentionClampsStaleContractCursor covers the per-contract half: the
// global checkpoint can be current while an individual contract cursor —
// written before the outage — still points into forgotten history. The
// cursor must be clamped to the same boundary or the fetch would still ask
// for ledgers the source cannot serve. The fetch itself fails, the way it
// does whenever a source has forgotten the range: that is what lets the test
// read back the clamped position instead of the ledger a completed page
// would have advanced it to.
func TestRetentionClampsStaleContractCursor(t *testing.T) {
	ctx := context.Background()
	rpc := &errorRPC{latest: 6100, oldest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 6050 // inside the window: the checkpoint is fine
	seedMonitor(st, `{"event_name": "transfer"}`)
	require.NoError(t, st.InitializeContractCursor(ctx, "", contractA, 1000))
	p := newResumePoller(rpc, st, &fakeDispatcher{})

	require.ErrorIs(t, p.Poll(ctx), errSourceDown,
		"the source cannot serve the clamped range, which is why the clamp matters")

	require.Empty(t, st.retentionGaps, "the checkpoint is inside the window, so there is no gap to record")
	require.Len(t, rpc.requests, 1)
	assert.Equal(t, uint32(6000), rpc.requests[0].StartLedger,
		"the stale contract cursor is clamped to the retained window")
	cursors, err := st.ListContractCursors(ctx, "")
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	assert.Equal(t, uint32(5999), cursors[0].LastLedger,
		"the cursor lands immediately before the oldest retained ledger")
}

// TestRetentionUnknownOldestIsUntouched covers a source that does not report
// a retention window (older nodes, SoroTrail): with nothing to compare
// against the poller neither jumps nor records, and polling continues from
// the checkpoint as always.
func TestRetentionUnknownOldestIsUntouched(t *testing.T) {
	rpc := &fakeRPC{latest: 6100, oldest: 0}
	st := newFakeStore()
	st.state.LastLedger = 100 // absurdly stale, but unverifiable
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newResumePoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Empty(t, st.retentionGaps, "an unknown retention window records nothing")
	require.Len(t, rpc.requests, 1)
	assert.Equal(t, uint32(101), rpc.requests[0].StartLedger,
		"without an oldestLedger to compare against, the resume point is untouched")
}
