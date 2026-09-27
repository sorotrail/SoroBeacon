package poller

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// --- fakeStore: the reorg-detection half of the Store interface ---

func (f *fakeStore) RecordLedgerHashes(_ context.Context, hashes []store.LedgerHash) error {
	for _, h := range hashes {
		f.ledgerHashes[h.Ledger] = h.Hash
	}
	return nil
}

func (f *fakeStore) LedgerHashes(_ context.Context, from, to uint32) ([]store.LedgerHash, error) {
	var out []store.LedgerHash
	for l := from; l <= to; l++ {
		if h, ok := f.ledgerHashes[l]; ok {
			out = append(out, store.LedgerHash{Ledger: l, Hash: h})
		}
	}
	return out, nil
}

func (f *fakeStore) PruneLedgerHashes(_ context.Context, before uint32) error {
	for l := range f.ledgerHashes {
		if l < before {
			delete(f.ledgerHashes, l)
		}
	}
	return nil
}

func (f *fakeStore) RetractAlertsFromLedger(_ context.Context, ledger uint32, at time.Time) (int64, error) {
	var n int64
	t := at.UTC()
	for i := range f.alerts {
		if f.alerts[i].Ledger >= ledger && f.alerts[i].RetractedAt == nil {
			f.alerts[i].RetractedAt = &t
			n++
		}
	}
	return n, nil
}

// reorgSource is a scripted EventSource that can also report ledger hashes, so
// a test can replay a chain and then rewrite one ledger's hash. It wraps an
// RPCSource (so it is a full EventSource) and overrides LedgerHashes with the
// scripted map; the embedded fakeRPC stays reachable through rpc.
type reorgSource struct {
	*RPCSource
	rpc    *fakeRPC
	hashes map[uint32]string
}

func newReorgSource(rpc *fakeRPC, hashes map[uint32]string) *reorgSource {
	return &reorgSource{
		RPCSource: NewRPCSource(rpc, stellar.DefaultDecoder{}),
		rpc:       rpc,
		hashes:    hashes,
	}
}

func (s *reorgSource) LedgerHashes(_ context.Context, from, to uint32) ([]store.LedgerHash, error) {
	var out []store.LedgerHash
	for l := from; l <= to; l++ {
		if h, ok := s.hashes[l]; ok {
			out = append(out, store.LedgerHash{Ledger: l, Hash: h})
		}
	}
	return out, nil
}

func newReorgPoller(src EventSource, st *fakeStore, d *fakeDispatcher, window, depth uint32) *Poller {
	return New(src, st, rules.NewRegistry(), d, 0, slog.New(slog.DiscardHandler)).
		WithReorg(window, depth)
}

// linearHashes builds a contiguous chain of distinct hashes up to tip.
func linearHashes(tip uint32) map[uint32]string {
	m := make(map[uint32]string, tip)
	for l := uint32(1); l <= tip; l++ {
		m[l] = fmt.Sprintf("hash-%d", l)
	}
	return m
}

// TestPollDetectsReorgAndRetractsAlerts is the core requirement: a ledger
// hash that changes under the poller marks the alerts derived from the
// orphaned range, without deleting them (a delivered alert cannot be unsent).
func TestPollDetectsReorgAndRetractsAlerts(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	st.state.LastLedger = 90
	seedMonitor(st, `{"event_name": "transfer"}`)

	src := newReorgSource(&fakeRPC{
		latest: 100,
		responses: []*stellar.GetEventsResult{{
			Events:       []stellar.Event{transferEvent("ev-1", 95, "1")},
			LatestLedger: 100,
		}},
	}, linearHashes(100))
	d := &fakeDispatcher{}
	p := newReorgPoller(src, st, d, 20, 0)

	require.NoError(t, p.Poll(ctx))
	require.Len(t, st.alerts, 1)
	assert.Nil(t, st.alerts[0].RetractedAt, "an alert on the canonical chain is not retracted")
	assert.Equal(t, uint32(95), st.alerts[0].Ledger, "the alert records the ledger it came from")
	require.Len(t, st.ledgerHashes, 20, "the tracking window is bounded by the configured depth")

	// The chain reorganises and rewrites ledger 95; the replacement chain has
	// no matching event yet.
	src.hashes[95] = "reorg-hash-95"
	src.rpc.responses = nil

	require.NoError(t, p.Poll(ctx))

	require.Len(t, st.alerts, 1, "the orphaned alert is kept, not deleted")
	require.NotNil(t, st.alerts[0].RetractedAt, "the orphaned alert must be marked retracted")
}

// TestPollReorgDetectionDisabledLeavesAlertsAlone pins the off switch: with
// window 0 the pre-feature behaviour is unchanged, so an upgraded deployment
// that turns detection off keeps ingesting without retractions.
func TestPollReorgDetectionDisabledLeavesAlertsAlone(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	st.state.LastLedger = 90
	seedMonitor(st, `{"event_name": "transfer"}`)

	src := newReorgSource(&fakeRPC{
		latest: 100,
		responses: []*stellar.GetEventsResult{{
			Events:       []stellar.Event{transferEvent("ev-1", 95, "1")},
			LatestLedger: 100,
		}},
	}, linearHashes(100))
	p := newReorgPoller(src, st, &fakeDispatcher{}, 0, 0)

	require.NoError(t, p.Poll(ctx))
	require.Len(t, st.alerts, 1)

	src.hashes[95] = "reorg-hash-95"
	src.rpc.responses = nil
	require.NoError(t, p.Poll(ctx))

	assert.Nil(t, st.alerts[0].RetractedAt, "detection is off, so nothing is retracted")
	assert.Empty(t, st.ledgerHashes, "no hashes are tracked when detection is off")
}

// TestPollReorgSourceWithoutHashesIsTolerated covers an EventSource that does
// not implement LedgerHashSource at all: the cycle must still ingest normally.
func TestPollReorgSourceWithoutHashesIsTolerated(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	st.state.LastLedger = 90
	seedMonitor(st, `{"event_name": "transfer"}`)

	rpc := &fakeRPC{
		latest: 100,
		responses: []*stellar.GetEventsResult{{
			Events:       []stellar.Event{transferEvent("ev-1", 95, "1")},
			LatestLedger: 100,
		}},
	}
	p := newReorgPoller(NewRPCSource(rpc, stellar.DefaultDecoder{}), st, &fakeDispatcher{}, 20, 0)

	require.NoError(t, p.Poll(ctx))
	require.Len(t, st.alerts, 1)
	assert.Nil(t, st.alerts[0].RetractedAt)
}

// TestPollConfirmationDepthHoldsUnconfirmedEvents proves the configurable
// latency: an event inside the confirmation depth is not evaluated until it is
// buried, and the checkpoint is held so the range is re-read.
func TestPollConfirmationDepthHoldsUnconfirmedEvents(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	st.state.LastLedger = 90
	seedMonitor(st, `{"event_name": "transfer"}`)

	src := newReorgSource(&fakeRPC{
		latest: 100,
		responses: []*stellar.GetEventsResult{{
			Events:       []stellar.Event{transferEvent("ev-1", 98, "1")},
			LatestLedger: 100,
		}},
	}, linearHashes(110))
	p := newReorgPoller(src, st, &fakeDispatcher{}, 20, 5)

	// depth 5 means only ledgers <= 95 are confirmed; the event at 98 is held.
	require.NoError(t, p.Poll(ctx))
	assert.Empty(t, st.alerts, "an unconfirmed event must not alert")
	assert.Equal(t, uint32(95), st.state.LastLedger, "the checkpoint is held at the confirmation boundary")

	// The chain advances; the same event is now buried five ledgers deep.
	src.rpc.latest = 110
	src.rpc.responses = []*stellar.GetEventsResult{{
		Events:       []stellar.Event{transferEvent("ev-1", 98, "1")},
		LatestLedger: 110,
	}}
	require.NoError(t, p.Poll(ctx))
	require.Len(t, st.alerts, 1, "the confirmed event alerts on the re-read")
	assert.Equal(t, "ev-1", st.alerts[0].EventID)
}
