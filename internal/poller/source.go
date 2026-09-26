package poller

import (
	"context"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// Watch is one contract the poller asked for, plus the server-side topic
// filter derived from that contract's enabled rules. A nil Topics means no
// safe filter could be derived (some rule matches unnamed events), so the
// source must return every event for the contract. Server-side filtering is
// only ever an optimisation: the poller still evaluates every returned event
// against every rule, and that client-side evaluation is the source of truth.
type Watch struct {
	ContractID string
	// Topics is a list of alternative topic filters (OR-ed together), each a
	// list of positional segment matchers with "*" and "**" wildcards.
	Topics [][]string
}

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
	// Watch is the full watch list for the cycle, identical on every call of
	// that cycle, so sources that batch (the RPC caps filters per request)
	// can encode batch position in the cursor. A source that cannot express
	// per-contract topic filters may ignore Watch[].Topics.
	//
	// The cursor is opaque to the poller; only the producing source may
	// interpret it. An empty NextCursor ends the cycle.
	FetchEvents(ctx context.Context, startLedger uint32, watch []Watch, cursor string, limit int) (FetchPage, error)
}

// RetentionReporter is implemented by EventSources that can report how far
// back their backing store still holds events. The live poller ignores it;
// the backfill job (internal/backfill) uses it to clamp a requested range to
// what the source can actually serve and to say so, instead of silently
// returning less. A source that does not implement it is treated as having
// unbounded history.
type RetentionReporter interface {
	// OldestLedger returns the oldest ledger the source can still serve, or 0
	// when it does not know. The RPC retains only ~1-7 days of events;
	// SoroTrail holds durable history.
	OldestLedger(ctx context.Context) (uint32, error)
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
