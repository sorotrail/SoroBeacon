package sorotrail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// watch builds the poller's watch list from bare contract IDs. These tests
// exercise the source's contract selection, not the topic filters, so no
// Topics are set.
func watch(ids ...string) []poller.Watch {
	out := make([]poller.Watch, 0, len(ids))
	for _, id := range ids {
		out = append(out, poller.Watch{ContractID: id})
	}
	return out
}

// stubSoroTrail serves scripted /events, /stats, and /health responses.
type stubSoroTrail struct {
	eventsJSON   string
	eventsStatus int
	statsJSON    string
	statsStatus  int
	healthJSON   string
	healthStatus int
	lastQuery    string
}

func (s *stubSoroTrail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/events":
		s.lastQuery = r.URL.RawQuery
		if s.eventsStatus != 0 {
			w.WriteHeader(s.eventsStatus)
		}
		_, _ = w.Write([]byte(s.eventsJSON))
	case "/api/v1/stats":
		if s.statsStatus != 0 {
			w.WriteHeader(s.statsStatus)
		}
		_, _ = w.Write([]byte(s.statsJSON))
	case "/api/v1/health":
		if s.healthJSON == "" && s.healthStatus == 0 {
			http.NotFound(w, r)
			return
		}
		if s.healthStatus != 0 {
			w.WriteHeader(s.healthStatus)
		}
		_, _ = w.Write([]byte(s.healthJSON))
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
	page, err := src.FetchEvents(context.Background(), 100, watch("CA7QYNF7", "CB..."), "", 50)
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
	endPage, err := src.FetchEvents(context.Background(), 100, watch("CA7QYNF7"), "c2", 50)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := srv.lastQuery, "contract_id=CA7QYNF7&cursor=c2&limit=50"; got != want {
		t.Fatalf("continuation query = %q, want %q", got, want)
	}
	if endPage.LatestLedger != 200 {
		t.Fatalf("empty final page tip = %d, want 200 (indexer tip)", endPage.LatestLedger)
	}
}

func TestSourceShortPageAdvancesToIndexerTip(t *testing.T) {
	srv := &stubSoroTrail{
		statsJSON: `{"oldest_stored_ledger": 100, "last_ingested_ledger": 250}`,
		eventsJSON: `{
			"events": [
				{
					"id": "ev-1",
					"contract_id": "CA7QYNF7",
					"ledger": 120,
					"tx_hash": "hash",
					"topics": [{"symbol": "transfer"}],
					"value": {"i128": "500"}
				}
			]
		}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := NewSource(NewClient(ts.URL, nil))
	page, err := src.FetchEvents(context.Background(), 100, watch("CA7QYNF7"), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(page.Events))
	}
	if page.LatestLedger != 250 {
		t.Fatalf("short page LatestLedger = %d, want 250 (indexer tip)", page.LatestLedger)
	}
	if page.NextCursor != "" {
		t.Fatalf("short page NextCursor = %q, want empty", page.NextCursor)
	}
}

func TestSourceLaggingUpstreamStatus(t *testing.T) {
	srv := &stubSoroTrail{
		statsJSON: `{
			"oldest_stored_ledger": 100,
			"last_ingested_ledger": 1000,
			"chain_head_ledger": 1500,
			"ingest_lag_ledgers": 500
		}`,
		eventsJSON: `{"events": []}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := NewSource(NewClient(ts.URL, nil))
	page, err := src.FetchEvents(context.Background(), 1000, watch("CA7QYNF7"), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.LatestLedger != 1000 {
		t.Fatalf("page LatestLedger = %d, want 1000", page.LatestLedger)
	}
	chainHead, upstreamLag, failures := src.UpstreamStatus()
	if chainHead != 1500 {
		t.Fatalf("chainHead = %d, want 1500", chainHead)
	}
	if upstreamLag != 500 {
		t.Fatalf("upstreamLag = %d, want 500", upstreamLag)
	}
	if failures != 0 {
		t.Fatalf("failures = %d, want 0", failures)
	}
}

func TestSourceFiltersRetractedEventsAndRecordsRetractions(t *testing.T) {
	srv := &stubSoroTrail{
		statsJSON: `{
			"oldest_stored_ledger": 100,
			"last_ingested_ledger": 200,
			"verified_through_ledger": 200
		}`,
		eventsJSON: `{
			"events": [
				{
					"id": "ev-retracted",
					"contract_id": "CA7QYNF7",
					"ledger": 180,
					"retracted": true,
					"topics": [{"symbol": "transfer"}]
				},
				{
					"id": "ev-canonical",
					"contract_id": "CA7QYNF7",
					"ledger": 181,
					"topics": [{"symbol": "transfer"}]
				}
			]
		}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := NewSource(NewClient(ts.URL, nil))
	page, err := src.FetchEvents(context.Background(), 175, watch("CA7QYNF7"), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != "ev-canonical" {
		t.Fatalf("expected only canonical event, got %+v", page.Events)
	}
	retractedFrom, retractedIDs := src.ConsumeRetractions()
	if retractedFrom != 180 {
		t.Fatalf("retractedFrom = %d, want 180", retractedFrom)
	}
	if len(retractedIDs) != 1 || retractedIDs[0] != "ev-retracted" {
		t.Fatalf("retractedIDs = %v, want [ev-retracted]", retractedIDs)
	}

	// Regression in verified_through_ledger also triggers retraction.
	srv.statsJSON = `{
		"oldest_stored_ledger": 100,
		"last_ingested_ledger": 195,
		"verified_through_ledger": 190
	}`
	srv.eventsJSON = `{"events": []}`
	if _, err := src.FetchEvents(context.Background(), 181, watch("CA7QYNF7"), "", 50); err != nil {
		t.Fatal(err)
	}
	retractedFrom, _ = src.ConsumeRetractions()
	if retractedFrom != 191 {
		t.Fatalf("regressed verified_through_ledger retractedFrom = %d, want 191", retractedFrom)
	}
}

func TestSourceInvalidCursorDetection(t *testing.T) {
	srv := &stubSoroTrail{
		statsJSON:    `{"first_ledger": 100, "last_ledger": 200}`,
		eventsStatus: http.StatusBadRequest,
		eventsJSON:   `{"error": "invalid cursor: expired"}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := NewSource(NewClient(ts.URL, nil))
	_, err := src.FetchEvents(context.Background(), 100, watch("CA7QYNF7"), "stale-cursor", 50)
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("err = %v, want ErrInvalidCursor", err)
	}

	// Stuck cursor (next_cursor identical to input cursor) is also rejected.
	srv.eventsStatus = http.StatusOK
	srv.eventsJSON = `{"events": [{"id": "ev-1", "contract_id": "CA7QYNF7", "ledger": 150}], "next_cursor": "stuck"}`
	_, err = src.FetchEvents(context.Background(), 100, watch("CA7QYNF7"), "stuck", 50)
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("stuck cursor err = %v, want ErrInvalidCursor", err)
	}
}

func TestClientHealthDegradedAndTransientRetry(t *testing.T) {
	srv := &stubSoroTrail{
		statsJSON:  `{"first_ledger": 100, "last_ledger": 200}`,
		healthJSON: `{"status": "degraded"}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := NewClient(ts.URL, nil).WithRetry(2, time.Millisecond)
	if _, err := c.GetHealth(context.Background()); err == nil {
		t.Fatal("degraded upstream must fail GetHealth")
	}
	if c.ConsecutiveFailures() == 0 {
		t.Fatal("consecutive failures should increment on degraded health")
	}

	// Transient 503 followed by 200 succeeds via bounded retry and resets failures.
	var calls atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": "warming up"}`))
			return
		}
		_, _ = w.Write([]byte(`{"first_ledger": 100, "last_ledger": 210}`))
	}))
	defer flaky.Close()

	fc := NewClient(flaky.URL, nil).WithRetry(2, time.Millisecond)
	st, err := fc.Stats(context.Background())
	if err != nil {
		t.Fatalf("transient 503 should succeed on retry, got %v", err)
	}
	if st.TipLedger() != 210 {
		t.Fatalf("tip = %d, want 210", st.TipLedger())
	}
	if fc.ConsecutiveFailures() != 0 {
		t.Fatalf("failures = %d, want 0 after recovery", fc.ConsecutiveFailures())
	}
}

func TestVerifyUpstreamRejectsWrongNetworkAndIncompatibleEndpoint(t *testing.T) {
	srv := &stubSoroTrail{
		statsJSON: `{"first_ledger": 100, "last_ledger": 200, "network_passphrase": "` + stellar.PassphraseMainnet + `"}`,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := NewClient(ts.URL, nil)
	err := c.VerifyUpstream(context.Background(), stellar.PassphraseTestnet)
	if err == nil {
		t.Fatal("expected network mismatch error")
	}
	for _, want := range []string{"network mismatch", "mainnet", "testnet"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should contain %q", err.Error(), want)
		}
	}

	// Source with expected passphrase refuses to evaluate events on poll.
	src := NewSource(c).WithExpectedPassphrase(stellar.PassphraseTestnet)
	if _, err := src.FetchEvents(context.Background(), 100, watch("CA7QYNF7"), "", 50); err == nil {
		t.Fatal("FetchEvents must fail when upstream network passphrase mismatches")
	}

	// Non-SoroTrail endpoint (unrelated JSON or HTML) fails with ErrIncompatibleUpstream.
	srv.statsJSON = `{"hello": "world"}`
	err = c.VerifyUpstream(context.Background(), stellar.PassphraseTestnet)
	if !errors.Is(err, ErrIncompatibleUpstream) {
		t.Fatalf("unrelated JSON err = %v, want ErrIncompatibleUpstream", err)
	}

	srv.statsJSON = `<html><body>not sorotrail</body></html>`
	err = c.VerifyUpstream(context.Background(), stellar.PassphraseTestnet)
	if !errors.Is(err, ErrIncompatibleUpstream) {
		t.Fatalf("HTML response err = %v, want ErrIncompatibleUpstream", err)
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

func TestClientNetworkPassphrase(t *testing.T) {
	srv := &stubSoroTrail{statsJSON: `{"first_ledger":100,"last_ledger":200,"network_passphrase":"test-passphrase"}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	passphrase, err := NewClient(ts.URL, nil).NetworkPassphrase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if passphrase != "test-passphrase" {
		t.Fatalf("network passphrase = %q, want test-passphrase", passphrase)
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
	page, err := src.FetchEvents(context.Background(), 1, watch("C"), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != "good" {
		t.Fatalf("want only the well-formed event, got %+v", page.Events)
	}
}

// Compile-time interface checks.
var _ poller.EventSource = (*Source)(nil)
var _ poller.UpstreamStatusSource = (*Source)(nil)

func TestEventsResponseShape(t *testing.T) {
	var r eventsResponse
	if err := json.Unmarshal([]byte(`{"events": [], "next_cursor": "x"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.NextCursor != "x" {
		t.Fatal("next_cursor not decoded")
	}
}
