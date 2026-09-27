package poller

// Coverage for the part of the ingest loop poller_test.go does not reach:
// stopping with a cursor saved and starting again from it. Both failure
// modes are silent — re-reading duplicates alerts, skipping loses them — so
// these tests pin the contract the poller actually provides: at-least-once.
// Poll persists the checkpoint (the minimum latestLedger the source
// reported) only after its page loop completes, so a crash anywhere inside
// the cycle leaves the checkpoint where it was and the whole range is
// replayed on the next start; the store's (rule_id, event_id) dedup guard
// absorbs the replay. A test that accepted skipped events would encode a
// bug; there is none here to accept.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// errSourceDown is the scripted source failure for the error tests.
var errSourceDown = errors.New("source down")

// newResumePoller wires a poller around any stellar.Client fake (newTestPoller
// above is pinned to the concrete fakeRPC type), so these tests can script
// crashes, errors and empty pages behind the same EventSource boundary.
func newResumePoller(client stellar.Client, st *fakeStore, d *fakeDispatcher) *Poller {
	return New(NewRPCSource(client, stellar.DefaultDecoder{}), st, rules.NewRegistry(), d, 0, slog.New(slog.DiscardHandler))
}

// healthy getters shared by the fakes below: only GetEvents behaviour
// differs between the restart scenarios.
func latestLedgerAt(sequence uint32) *stellar.LatestLedger {
	return &stellar.LatestLedger{Sequence: sequence}
}
func healthyHealth() *stellar.Health { return &stellar.Health{Status: "healthy"} }
func anyNetwork() *stellar.Network   { return &stellar.Network{} }

// crashRPC serves one full page, then fails the follow-up cursor request —
// the shape of dying mid-cycle, between fetching pages and persisting.
type crashRPC struct {
	fullPage stellar.GetEventsResult
	failOn   int // which GetEvents call fails (0-indexed)
	latest   uint32

	requests []stellar.GetEventsRequest
}

func (f *crashRPC) GetEvents(_ context.Context, req stellar.GetEventsRequest) (*stellar.GetEventsResult, error) {
	f.requests = append(f.requests, req)
	if len(f.requests)-1 == f.failOn {
		return nil, errSourceDown
	}
	return &f.fullPage, nil
}

func (f *crashRPC) GetLatestLedger(context.Context) (*stellar.LatestLedger, error) {
	return latestLedgerAt(f.latest), nil
}
func (f *crashRPC) GetHealth(context.Context) (*stellar.Health, error) { return healthyHealth(), nil }
func (f *crashRPC) GetNetwork(context.Context) (*stellar.Network, error) {
	return anyNetwork(), nil
}

// errorRPC fails every GetEvents call and still records the requests it was
// asked to make, so a test can assert the cursor did not move by watching
// what the next cycle asks for.
type errorRPC struct {
	latest   uint32
	requests []stellar.GetEventsRequest
}

func (f *errorRPC) GetEvents(_ context.Context, req stellar.GetEventsRequest) (*stellar.GetEventsResult, error) {
	f.requests = append(f.requests, req)
	return nil, errSourceDown
}

func (f *errorRPC) GetLatestLedger(context.Context) (*stellar.LatestLedger, error) {
	return latestLedgerAt(f.latest), nil
}
func (f *errorRPC) GetHealth(context.Context) (*stellar.Health, error) { return healthyHealth(), nil }
func (f *errorRPC) GetNetwork(context.Context) (*stellar.Network, error) {
	return anyNetwork(), nil
}

// emptyRPC answers every request with a short, eventless page at latest.
type emptyRPC struct {
	latest uint32
}

func (f *emptyRPC) GetEvents(context.Context, stellar.GetEventsRequest) (*stellar.GetEventsResult, error) {
	return &stellar.GetEventsResult{LatestLedger: f.latest}, nil
}

func (f *emptyRPC) GetLatestLedger(context.Context) (*stellar.LatestLedger, error) {
	return latestLedgerAt(f.latest), nil
}
func (f *emptyRPC) GetHealth(context.Context) (*stellar.Health, error) { return healthyHealth(), nil }
func (f *emptyRPC) GetNetwork(context.Context) (*stellar.Network, error) {
	return anyNetwork(), nil
}

// fullCrashPage builds one max-size page of distinct matching events, so the
// source reports a cursor continuation and the poller must come back for it
// before the cycle can complete.
func fullCrashPage(latest uint32) stellar.GetEventsResult {
	events := make([]stellar.Event, stellar.DefaultEventsLimit)
	for i := range events {
		events[i] = transferEvent(fmt.Sprintf("ev-crash-%03d", i), latest-10, "1")
	}
	return stellar.GetEventsResult{
		Events:       events,
		LatestLedger: latest,
		Cursor:       "not-drained-yet",
	}
}

// TestResumePersistsCursorAfterProcessing pins the persist half: after a
// successful cycle the checkpoint reflects what the source reported, so a
// restart can neither re-read nor skip anything before it.
func TestResumePersistsCursorAfterProcessing(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newResumePoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	assert.Equal(t, uint32(6000), st.state.LastLedger,
		"the checkpoint must equal the latestLedger the cycle processed")
	assert.Empty(t, st.state.LastCursor,
		"a completed cycle leaves no cursor continuation behind")
}

// TestResumeStartsFromPersistedCursor pins the resume half: a fresh poller
// over saved state continues just after the checkpoint instead of starting
// over or cold-starting from the tip.
func TestResumeStartsFromPersistedCursor(t *testing.T) {
	rpc := &fakeRPC{
		latest: 6000,
		responses: []*stellar.GetEventsResult{
			{LatestLedger: 5600}, // first cycle closes at 5600
			{LatestLedger: 6000}, // second cycle closes at the tip
		},
	}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newResumePoller(rpc, st, &fakeDispatcher{})
	require.NoError(t, p.Poll(context.Background()))
	require.Equal(t, uint32(5600), st.state.LastLedger)

	resumed := newResumePoller(rpc, st, &fakeDispatcher{})
	require.NoError(t, resumed.Poll(context.Background()))

	require.Len(t, rpc.requests, 2)
	assert.Equal(t, uint32(5601), rpc.requests[1].StartLedger,
		"the restart resumes just after the persisted checkpoint")
}

// TestResumeCrashMidCycleDoesNotSkipEvents is the important one: dying
// between fetching pages and persisting must not advance the checkpoint past
// unprocessed events. The restart therefore re-polls the whole range —
// at-least-once — and the dedup guard keeps that replay from duplicating
// alerts. The cursor would be allowed to sit still here; it must never jump.
func TestResumeCrashMidCycleDoesNotSkipEvents(t *testing.T) {
	// Cycle 1: a full page (so the source reports a continuation), then the
	// cursor request fails — the poll dies before persisting anything.
	crash := &crashRPC{fullPage: fullCrashPage(6000), failOn: 1, latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	d1 := &fakeDispatcher{}
	p := newResumePoller(crash, st, d1)

	err := p.Poll(context.Background())
	require.ErrorIs(t, err, errSourceDown)

	assert.Equal(t, uint32(5500), st.state.LastLedger,
		"a crashed cycle must not advance the cursor past unprocessed events")
	assert.Empty(t, st.state.LastCursor)
	require.Len(t, d1.dispatched, stellar.DefaultEventsLimit,
		"the fetched events were already dispatched when the crash hit")

	// Cycle 2: a fresh poller over the unchanged checkpoint re-polls the
	// same range and then drains the page that was never fetched.
	replay := fullCrashPage(6000)
	rpc2 := &fakeRPC{
		latest: 6000,
		responses: []*stellar.GetEventsResult{
			{Events: replay.Events, LatestLedger: 6000, Cursor: "not-drained-yet"},
			{Events: []stellar.Event{transferEvent("ev-tail", 5999, "1")}, LatestLedger: 6000},
		},
	}
	d2 := &fakeDispatcher{}
	resumed := newResumePoller(rpc2, st, d2)
	require.NoError(t, resumed.Poll(context.Background()))

	require.NotEmpty(t, rpc2.requests)
	assert.Equal(t, uint32(5501), rpc2.requests[0].StartLedger,
		"the restart re-polls from the old checkpoint: replay, not skip")

	// Every event of the replayed range plus the tail page alerted exactly
	// once: the replay was absorbed by the dedup guard, not duplicated.
	require.Len(t, st.alerts, stellar.DefaultEventsLimit+1)
	seen := make(map[string]bool, len(st.alerts))
	for _, a := range st.alerts {
		assert.False(t, seen[a.EventID], "event %s alerted twice after replay", a.EventID)
		seen[a.EventID] = true
	}
	assert.Len(t, d2.dispatched, 1,
		"only the unseen tail event is dispatched by the restart")
}

// TestResumeEmptyPageKeepsPosition pins that a page with no events does not
// stall the checkpoint or rewind it: the cycle closes at the source's
// latestLedger so the next start continues after it.
func TestResumeEmptyPageKeepsPosition(t *testing.T) {
	rpc := &emptyRPC{latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newResumePoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	assert.Equal(t, uint32(6000), st.state.LastLedger,
		"an empty page closes the cycle at the source's latestLedger")
	assert.Empty(t, st.state.LastCursor)
	assert.Empty(t, st.alerts)

	// A second cycle over the closed position neither rewinds the checkpoint
	// nor re-polls anything before it: the position is kept, not lost.
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, uint32(6000), st.state.LastLedger,
		"an empty cycle leaves the checkpoint standing, never rewound")
	assert.Empty(t, st.alerts)
}

// TestResumeSourceErrorLeavesCursorAtPersistedPosition pins the error half:
// a source failure aborts the cycle before any persistence, so the
// checkpoint stays where the last successful cycle left it. Recovery resumes
// from that same position rather than jumping to the tip and skipping the
// events the errors covered.
func TestResumeSourceErrorLeavesCursorAtPersistedPosition(t *testing.T) {
	rpc := &errorRPC{latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newResumePoller(rpc, st, &fakeDispatcher{})

	require.ErrorIs(t, p.Poll(context.Background()), errSourceDown)

	assert.Equal(t, uint32(5500), st.state.LastLedger,
		"a failed cycle must leave the checkpoint untouched")
	assert.Empty(t, st.state.LastCursor)
	assert.Empty(t, st.alerts)

	// Recovery: with the source healthy again, the next cycle resumes just
	// after the still-standing checkpoint, covering the error window.
	healthy := &fakeRPC{latest: 6000}
	require.NoError(t, newResumePoller(healthy, st, &fakeDispatcher{}).Poll(context.Background()))
	require.NotEmpty(t, healthy.requests)
	assert.Equal(t, uint32(5501), healthy.requests[0].StartLedger,
		"recovery resumes after the checkpoint the errors left untouched")
	assert.Equal(t, uint32(6000), st.state.LastLedger,
		"the recovered cycle closes at the source's tip")
}
