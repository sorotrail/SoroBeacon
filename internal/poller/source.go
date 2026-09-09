package poller

import (
	"context"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// EventSource is where events come from. The poller itself only knows this
// interface, so the ingest loop is identical regardless of backend:
//
//   - rpcsource (default): polls a Stellar RPC node's getEvents directly
//     and decodes locally.
//   - sorotrail (upstream mode): reads from a SoroTrail indexer's HTTP
//     API. SoroTrail stores events durably past the RPC's ~1-7 day
//     retention window, so an upstream-mode SoroBeacon can monitor history
//     the RPC has already dropped — and several SoroBeacon instances can
//     share one indexer instead of each polling the chain.
//
// Contributors: implement this interface and wire it in cmd/sorobeacon to
// add a backend. Nothing in the poller should need to change.
type EventSource interface {
	// LatestLedger returns the chain tip as the source knows it, used for
	// cold starts.
	LatestLedger(ctx context.Context) (uint32, error)

	// FetchEvents returns one page of decoded events. The first call for a
	// cycle passes StartLedger and an empty Cursor; continuation calls pass
	// the Cursor returned by the previous page, with StartLedger ignored.
	// Contracts is the full watch list for the cycle, identical on every
	// call of that cycle, so sources that batch (the RPC caps filters per
	// request) can encode batch position in the cursor.
	//
	// The cursor is opaque to the poller; only the producing source may
	// interpret it. An empty NextCursor ends the cycle.
	FetchEvents(ctx context.Context, startLedger uint32, contracts []string, cursor string, limit int) (FetchPage, error)
}

// FetchPage is one page of events from an EventSource.
type FetchPage struct {
	// Events are already decoded — sources handle their own decoding, so
	// the poller never sees raw XDR.
	Events []*stellar.DecodedEvent
	// LatestLedger is the source's notion of the chain tip during this
	// page; the poller takes the minimum across the cycle as its
	// checkpoint.
	LatestLedger uint32
	// NextCursor continues the cycle, or empty when the source has no more
	// events for the requested range.
	NextCursor string
}
