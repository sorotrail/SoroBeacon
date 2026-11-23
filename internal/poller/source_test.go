package poller

import (
	"context"
	"errors"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// fakeSource is a scripted EventSource: each FetchEvents call consumes the
// next page, so a test can stage empty, continuing and final pages without a
// network. The script is the executable form of the interface contract in
// source.go — anyone adding a backend can read this to learn what the
// poller expects at each step of a cycle.
type fakeSource struct {
	pages []FetchPage
	err   error
	calls int
	// last records the arguments of the most recent call, so tests can
	// assert the cursor handoff between pages.
	last struct {
		startLedger uint32
		cursor      string
		limit       int
	}
}

func (f *fakeSource) LatestLedger(ctx context.Context) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	return 999, nil
}

func (f *fakeSource) FetchEvents(ctx context.Context, startLedger uint32, watch []Watch, cursor string, limit int) (FetchPage, error) {
	f.calls++
	f.last.startLedger = startLedger
	f.last.cursor = cursor
	f.last.limit = limit
	if f.err != nil {
		return FetchPage{}, f.err
	}
	if len(f.pages) == 0 {
		return FetchPage{}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

func TestSourceEmptyPage(t *testing.T) {
	// An empty page with no cursor ends the cycle immediately: the poller
	// must not treat "no events" as an error or loop asking for more.
	src := &fakeSource{pages: []FetchPage{{Events: nil, LatestLedger: 100}}}
	page, err := src.FetchEvents(context.Background(), 90, nil, "", 100)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(page.Events) != 0 {
		t.Errorf("Events = %d, want 0", len(page.Events))
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty (cycle ends)", page.NextCursor)
	}
	if page.LatestLedger != 100 {
		t.Errorf("LatestLedger = %d, want 100", page.LatestLedger)
	}
}

func TestSourceContinuingPage(t *testing.T) {
	// A page carrying NextCursor continues the cycle: the next call must
	// hand that cursor back, with the cursor — not StartLedger — selecting
	// the position.
	ev := &stellar.DecodedEvent{ID: "ev-1", ContractID: "CABC", Ledger: 91}
	src := &fakeSource{pages: []FetchPage{
		{Events: []*stellar.DecodedEvent{ev}, LatestLedger: 100, NextCursor: "cursor-1"},
		{Events: nil, LatestLedger: 100},
	}}
	first, err := src.FetchEvents(context.Background(), 90, nil, "", 100)
	if err != nil {
		t.Fatalf("first FetchEvents: %v", err)
	}
	if first.NextCursor != "cursor-1" {
		t.Fatalf("NextCursor = %q, want cursor-1", first.NextCursor)
	}
	second, err := src.FetchEvents(context.Background(), 90, nil, first.NextCursor, 100)
	if err != nil {
		t.Fatalf("second FetchEvents: %v", err)
	}
	if src.last.cursor != "cursor-1" {
		t.Errorf("continuation cursor = %q, want cursor-1", src.last.cursor)
	}
	if second.NextCursor != "" {
		t.Errorf("final NextCursor = %q, want empty", second.NextCursor)
	}
	if src.calls != 2 {
		t.Errorf("calls = %d, want 2", src.calls)
	}
}

func TestSourceFinalPage(t *testing.T) {
	// A page with events but no cursor is complete in itself: one call
	// drains the cycle and the events are already decoded.
	events := []*stellar.DecodedEvent{
		{ID: "ev-1", ContractID: "CABC", Ledger: 91},
		{ID: "ev-2", ContractID: "CABC", Ledger: 92},
	}
	src := &fakeSource{pages: []FetchPage{{Events: events, LatestLedger: 100}}}
	page, err := src.FetchEvents(context.Background(), 90, nil, "", 100)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("Events = %d, want 2", len(page.Events))
	}
	for _, ev := range page.Events {
		if ev.ID == "" || ev.ContractID == "" {
			t.Errorf("event arrived undecoded: %+v", ev)
		}
	}
}

func TestSourceErrorPropagates(t *testing.T) {
	// A failing source must surface its error, never masquerade as an
	// empty page: the poller backs off and retries on error but would
	// advance its checkpoint past unprocessed ledgers on empty.
	want := errors.New("rpc unavailable")
	src := &fakeSource{err: want}
	_, err := src.FetchEvents(context.Background(), 90, nil, "", 100)
	if !errors.Is(err, want) {
		t.Errorf("FetchEvents err = %v, want %v", err, want)
	}
	if _, err := src.LatestLedger(context.Background()); !errors.Is(err, want) {
		t.Errorf("LatestLedger err = %v, want %v", err, want)
	}
}

func TestSourceImplementsInterface(t *testing.T) {
	// Compile-time proof the fake satisfies the seam new backends
	// implement; if EventSource gains a method, this breaks loudly here
	// instead of silently drifting from the contract.
	var _ EventSource = (*fakeSource)(nil)
}
