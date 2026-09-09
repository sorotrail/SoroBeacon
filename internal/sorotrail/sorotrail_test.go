package sorotrail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/poller"
)

// stubSoroTrail serves scripted /events and /stats responses.
type stubSoroTrail struct {
	eventsJSON string
	statsJSON  string
	lastQuery  string
}

func (s *stubSoroTrail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.lastQuery = r.URL.RawQuery
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/events":
		_, _ = w.Write([]byte(s.eventsJSON))
	case "/api/v1/stats":
		_, _ = w.Write([]byte(s.statsJSON))
	default:
		http.NotFound(w, r)
	}
}

func TestSourceFetchEvents(t *testing.T) {
	srv := &stubSoroTrail{
		statsJSON: `{"total_events": 10, "first_ledger": 100, "last_ledger": 200}`,
		eventsJSON: `{
			"events": [
				{
					"id": "ev-1",
					"contract_id": "CA7QYNF7",
					"ledger": 150,
					"tx_hash": "hash",
					"topics": [{"symbol": "transfer"}, {"address": "GA..."}],
					"value": {"i128": "1000"},
					"ledger_closed_at": "2026-09-01T00:00:00Z"
				}
			],
			"next_cursor": "c2"
		}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := NewSource(NewClient(ts.URL, nil))

	// First page: from_ledger applies, contract union passed through.
	page, err := src.FetchEvents(context.Background(), 100, []string{"CA7QYNF7", "CB..."}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(page.Events))
	}
	ev := page.Events[0]
	if ev.ID != "ev-1" || ev.ContractID != "CA7QYNF7" || ev.Ledger != 150 {
		t.Fatalf("event mapped wrong: %+v", ev)
	}
	if ev.EventName() != "transfer" {
		t.Fatalf("event name = %q, want transfer", ev.EventName())
	}
	if page.NextCursor != "c2" {
		t.Fatalf("cursor = %q, want c2", page.NextCursor)
	}
	if page.LatestLedger != 150 {
		t.Fatalf("page tip = %d, want 150 (newest event ledger)", page.LatestLedger)
	}

	// Value decoded through to the generic shape.
	if _, ok := ev.Value.(map[string]any); !ok {
		t.Fatalf("value not decoded: %#v", ev.Value)
	}

	// First call must carry from_ledger and the comma-joined union.
	q := srv.lastQuery
	if want := "contract_id=CA7QYNF7%2CCB...&from_ledger=100&limit=50"; q != want {
		t.Fatalf("query = %q, want %q", q, want)
	}

	// Continuation: cursor passes through, from_ledger is not sent.
	srv.eventsJSON = `{"events": []}`
	if _, err := src.FetchEvents(context.Background(), 100, []string{"CA7QYNF7"}, "c2", 50); err != nil {
		t.Fatal(err)
	}
	if got, want := srv.lastQuery, "contract_id=CA7QYNF7&cursor=c2&limit=50"; got != want {
		t.Fatalf("continuation query = %q, want %q", got, want)
	}
}

func TestSourceLatestLedger(t *testing.T) {
	srv := &stubSoroTrail{statsJSON: `{"first_ledger": 100, "last_ledger": 200}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := NewSource(NewClient(ts.URL, nil))
	latest, err := src.LatestLedger(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest != 200 {
		t.Fatalf("latest = %d, want 200", latest)
	}
}

func TestSourceLatestLedgerEmptyIndexer(t *testing.T) {
	srv := &stubSoroTrail{statsJSON: `{"first_ledger": 0, "last_ledger": 0}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := NewSource(NewClient(ts.URL, nil))
	if _, err := src.LatestLedger(context.Background()); err == nil {
		t.Fatal("an indexer with no ledgers should be an error, not a silent 0")
	}
}

func TestClientHealth(t *testing.T) {
	srv := &stubSoroTrail{statsJSON: `{"first_ledger": 100, "last_ledger": 200}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := NewClient(ts.URL, nil)
	h, err := c.GetHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.LatestLedger != 200 {
		t.Fatalf("health ledger = %d, want 200", h.LatestLedger)
	}
}

func TestSourceSkipsMalformedEvent(t *testing.T) {
	srv := &stubSoroTrail{
		eventsJSON: `{
			"events": [
				{"id": "bad", "contract_id": "C", "ledger": 1, "topics": 42},
				{"id": "good", "contract_id": "C", "ledger": 2, "topics": [{"symbol": "transfer"}]}
			]
		}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := NewSource(NewClient(ts.URL, nil))
	page, err := src.FetchEvents(context.Background(), 1, []string{"C"}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != "good" {
		t.Fatalf("want only the well-formed event, got %+v", page.Events)
	}
}

// Compile-time interface checks.
var _ poller.EventSource = (*Source)(nil)

func TestEventsResponseShape(t *testing.T) {
	var r eventsResponse
	if err := json.Unmarshal([]byte(`{"events": [], "next_cursor": "x"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.NextCursor != "x" {
		t.Fatal("next_cursor not decoded")
	}
}
