package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/alerts"
	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// decodedTransfer decodes one scripted transfer event, so the tests exercise
// the same event shape the RPC source hands the Ingestor.
func decodedTransfer(t *testing.T, id string) *stellar.DecodedEvent {
	t.Helper()
	ev, err := stellar.DefaultDecoder{}.DecodeEvent(context.Background(), transferEvent(id, 5990, "100"))
	require.NoError(t, err)
	return ev
}

func newOptionsIngestor(st *fakeStore, d *fakeDispatcher, log *slog.Logger) *Ingestor {
	return NewIngestor(st, rules.NewRegistry(), d, log)
}

// TestIngestorPublisherReceivesCreatedAlert covers the hook itself: every
// created alert reaches the live fan-out with the fields a dashboard row
// needs. With no publisher wired, ingestion still creates and dispatches (the
// nil-hook case covered below).
func TestIngestorPublisherReceivesCreatedAlert(t *testing.T) {
	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	live := broadcast.New(4)
	sub := live.Subscribe(1)
	defer sub.Close()
	d := &fakeDispatcher{}
	ing := newOptionsIngestor(st, d, slog.New(slog.DiscardHandler)).WithPublisher(live)

	res := ing.Handle(context.Background(), decodedTransfer(t, "ev-1"), st.monitors, HandleOptions{Deliver: true})

	require.Equal(t, 1, res.Alerts)
	require.Equal(t, 1, res.Dispatched)
	select {
	case got := <-sub.C:
		assert.Equal(t, st.alerts[0].ID, got.ID)
		assert.Equal(t, int64(1), got.MonitorID)
		assert.Equal(t, "m1", got.MonitorName)
		assert.Equal(t, "ev-1", got.EventID)
		assert.Empty(t, got.Enrichment)
	case <-time.After(time.Second):
		t.Fatal("a created alert did not reach the publisher")
	}
}

// TestIngestorWithoutPublisherStillIngests pins the nil hook as safe: a
// backfill-style Ingestor has no fan-out and must behave exactly as before.
func TestIngestorWithoutPublisherStillIngests(t *testing.T) {
	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	d := &fakeDispatcher{}
	ing := newOptionsIngestor(st, d, slog.New(slog.DiscardHandler))

	res := ing.Handle(context.Background(), decodedTransfer(t, "ev-1"), st.monitors, HandleOptions{Deliver: true})

	assert.Equal(t, 1, res.Alerts)
	assert.Equal(t, 1, res.Dispatched)
	require.Len(t, st.alerts, 1)
	require.Len(t, d.dispatched, 1)
}

// TestIngestorPublisherDropsOldestWhenQueueIsFullWithoutBlocking pins the
// publisher's failure policy. broadcast.Publish has no error to return: it
// never blocks, and a full subscriber queue loses its oldest pending event so
// the newest still gets through, counted by Dropped. Ingestion must finish
// even though nobody is reading the queue.
func TestIngestorPublisherDropsOldestWhenQueueIsFullWithoutBlocking(t *testing.T) {
	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	live := broadcast.New(1)
	sub := live.Subscribe(0)
	defer sub.Close()
	ing := newOptionsIngestor(st, &fakeDispatcher{}, slog.New(slog.DiscardHandler)).WithPublisher(live)

	// Decode before starting the goroutine: require.NoError calls FailNow,
	// which must run on the test goroutine.
	events := []*stellar.DecodedEvent{
		decodedTransfer(t, "ev-1"),
		decodedTransfer(t, "ev-2"),
		decodedTransfer(t, "ev-3"),
	}
	done := make(chan HandleResult, 1)
	go func() {
		var total HandleResult
		for _, ev := range events {
			res := ing.Handle(context.Background(), ev, st.monitors, HandleOptions{})
			total.Alerts += res.Alerts
		}
		done <- total
	}()

	select {
	case total := <-done:
		assert.Equal(t, 3, total.Alerts, "every alert must still be created")
	case <-time.After(2 * time.Second):
		t.Fatal("a full subscriber queue blocked ingestion")
	}

	assert.EqualValues(t, 2, live.Dropped(), "each overwrite of the full queue is counted")
	select {
	case got := <-sub.C:
		assert.Equal(t, "ev-3", got.EventID, "the newest alert must survive the drop-oldest policy")
	case <-time.After(time.Second):
		t.Fatal("the newest alert was not queued")
	}
}

// TestIngestorEnricherAddsContextToStoredDispatchedAndPublishedAlerts covers
// the success path: the operator endpoint is called with the contract and
// event name, and the result travels to the database row, the delivery and
// the live stream.
func TestIngestorEnricherAddsContextToStoredDispatchedAndPublishedAlerts(t *testing.T) {
	type enrichmentCall struct {
		contractID string
		eventName  string
	}
	calls := make(chan enrichmentCall, 1)
	const enrichment = `{"owner":"team-a","runbook":"https://runbooks.example/transfer"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- enrichmentCall{
			contractID: r.URL.Query().Get("contract_id"),
			eventName:  r.URL.Query().Get("event_name"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, enrichment)
	}))
	t.Cleanup(srv.Close)

	enricher, err := alerts.NewEnricher(srv.URL, time.Second, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, enricher)

	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	live := broadcast.New(4)
	sub := live.Subscribe(0)
	defer sub.Close()
	d := &fakeDispatcher{}
	ing := newOptionsIngestor(st, d, slog.New(slog.DiscardHandler)).
		WithPublisher(live).
		WithEnricher(enricher)

	res := ing.Handle(context.Background(), decodedTransfer(t, "ev-1"), st.monitors, HandleOptions{Deliver: true})
	require.Equal(t, 1, res.Alerts)

	var call enrichmentCall
	select {
	case call = <-calls:
	case <-time.After(time.Second):
		t.Fatal("the enricher was not called for a matched alert")
	}
	assert.Equal(t, contractA, call.contractID)
	assert.Equal(t, "transfer", call.eventName)

	require.Len(t, st.alerts, 1)
	// The enricher marks what it stored as operator supplied.
	stored := withOperatorSource(t, enrichment)
	assert.JSONEq(t, stored, string(st.alerts[0].Enrichment))
	require.Len(t, d.dispatched, 1)
	assert.JSONEq(t, stored, string(d.dispatched[0].Enrichment))

	select {
	case got := <-sub.C:
		assert.JSONEq(t, stored, string(got.Enrichment))
	case <-time.After(time.Second):
		t.Fatal("the enriched alert did not reach the publisher")
	}
}

// TestIngestorEnricherFailureStillCreatesAndDispatches pins the soft-failure
// policy: an enrichment error is logged and the alert is delivered without
// enrichment. Dropping the alert instead would lose a real alert because a
// context endpoint was down.
func TestIngestorEnricherFailureStillCreatesAndDispatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "enrichment source unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	enricher, err := alerts.NewEnricher(srv.URL, time.Second, time.Minute)
	require.NoError(t, err)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	d := &fakeDispatcher{}
	ing := newOptionsIngestor(st, d, log).WithEnricher(enricher)

	res := ing.Handle(context.Background(), decodedTransfer(t, "ev-1"), st.monitors, HandleOptions{Deliver: true})

	require.Equal(t, 1, res.Alerts)
	require.Equal(t, 1, res.Dispatched)
	require.Len(t, st.alerts, 1)
	assert.Empty(t, st.alerts[0].Enrichment)
	require.Len(t, d.dispatched, 1)
	assert.Empty(t, d.dispatched[0].Enrichment)
	assert.Contains(t, logs.String(), "alert enrichment failed",
		"a failing optional hook must be observable, or it reads as 'nothing to alert about'")
}

// TestIngestorEnricherTimeoutIsBounded covers the slow-hook case: the
// enricher's client timeout bounds the call, so a context endpoint that never
// answers cannot stall ingestion indefinitely.
func TestIngestorEnricherTimeoutIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	enricher, err := alerts.NewEnricher(srv.URL, 50*time.Millisecond, time.Minute)
	require.NoError(t, err)

	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	d := &fakeDispatcher{}
	ing := newOptionsIngestor(st, d, slog.New(slog.DiscardHandler)).WithEnricher(enricher)

	start := time.Now()
	res := ing.Handle(context.Background(), decodedTransfer(t, "ev-1"), st.monitors, HandleOptions{Deliver: true})
	elapsed := time.Since(start)

	assert.Less(t, elapsed, time.Second, "a blocked enrichment endpoint must not stall the poller")
	assert.Equal(t, 1, res.Alerts, "the alert is still created without enrichment")
	assert.Empty(t, st.alerts[0].Enrichment)
}

// TestIngestorCanceledContextDoesNotWaitOnEnricher pins cancellation: a
// cancelled context makes the enrichment request fail immediately rather than
// waiting for the hook.
func TestIngestorCanceledContextDoesNotWaitOnEnricher(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	enricher, err := alerts.NewEnricher(srv.URL, 5*time.Second, time.Minute)
	require.NoError(t, err)

	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	ing := newOptionsIngestor(st, &fakeDispatcher{}, slog.New(slog.DiscardHandler)).WithEnricher(enricher)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	event := decodedTransfer(t, "ev-1")

	done := make(chan HandleResult, 1)
	go func() {
		done <- ing.Handle(ctx, event, st.monitors, HandleOptions{Deliver: true})
	}()

	select {
	case res := <-done:
		assert.Equal(t, 1, res.Alerts)
	case <-time.After(time.Second):
		t.Fatal("a cancelled context still waited on the enrichment hook")
	}
	assert.Empty(t, st.alerts[0].Enrichment)
}

// TestIngestorEnrichesOnlyAfterARuleMatches pins the ordering the code
// actually implements: enrichment runs after a rule matched, so its result is
// notification context and can never influence rule evaluation. The issue's
// premise (enrich before evaluating) is not what the poller does; this test
// records the difference so a future change cannot flip the order silently.
func TestIngestorEnrichesOnlyAfterARuleMatches(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"owner":"team-a"}`)
	}))
	t.Cleanup(srv.Close)

	enricher, err := alerts.NewEnricher(srv.URL, time.Second, time.Minute)
	require.NoError(t, err)

	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	ing := newOptionsIngestor(st, &fakeDispatcher{}, slog.New(slog.DiscardHandler)).WithEnricher(enricher)

	nonMatching := &stellar.DecodedEvent{
		ID:             "ev-mint",
		ContractID:     contractA,
		Ledger:         5990,
		LedgerClosedAt: time.Unix(1_700_000_000, 0).UTC(),
		Topics:         []any{"mint"},
	}
	res := ing.Handle(context.Background(), nonMatching, st.monitors, HandleOptions{Deliver: true})
	assert.Zero(t, res.Matched)
	assert.Zero(t, res.Alerts)
	assert.Zero(t, hits.Load(), "an event no rule matched must not reach the enricher")

	res = ing.Handle(context.Background(), decodedTransfer(t, "ev-transfer"), st.monitors, HandleOptions{Deliver: true})
	assert.Equal(t, 1, res.Matched)
	assert.EqualValues(t, 1, hits.Load(), "a matched event is enriched once")
}

// withOperatorSource returns doc with the "_source": "operator" marker the
// enricher adds to everything it fetches.
func withOperatorSource(t *testing.T, doc string) string {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(doc), &m))
	m["_source"] = "operator"
	out, err := json.Marshal(m)
	require.NoError(t, err)
	return string(out)
}
