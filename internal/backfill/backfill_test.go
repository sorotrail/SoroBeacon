package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// contractID builds a valid contract strkey from a seed byte; WatchesFor drops
// invalid IDs, so test fixtures must be real strkeys.
func contractID(seed byte) string {
	var raw [32]byte
	for i := range raw {
		raw[i] = seed
	}
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		panic(err)
	}
	return s
}

var contractA = contractID(0xA1)

// fakeSource serves scripted pages keyed by the incoming cursor, and records
// what each call asked for so tests can pin the range and resume point.
type fakeSource struct {
	tip     uint32
	pages   map[string]poller.FetchPage
	failOn  int // 1-based call number that should fail; 0 never fails
	calls   int
	starts  []uint32
	cursors []string
}

func (f *fakeSource) LatestLedger(context.Context) (uint32, error) { return f.tip, nil }

func (f *fakeSource) FetchEvents(_ context.Context, startLedger uint32, _ []poller.Watch, cursor string, _ int) (poller.FetchPage, error) {
	f.calls++
	f.starts = append(f.starts, startLedger)
	f.cursors = append(f.cursors, cursor)
	if f.failOn > 0 && f.calls == f.failOn {
		return poller.FetchPage{}, errors.New("simulated interruption")
	}
	return f.pages[cursor], nil
}

// retainingSource adds the optional RetentionReporter seam to fakeSource, so
// the clamp behaviour is exercised without changing every test's source.
type retainingSource struct {
	*fakeSource
	oldest uint32
}

func (s retainingSource) OldestLedger(context.Context) (uint32, error) { return s.oldest, nil }

// fakeDispatcher records delivered alerts.
type fakeDispatcher struct {
	dispatched []notify.Alert
}

func (f *fakeDispatcher) Dispatch(_ context.Context, a notify.Alert) {
	f.dispatched = append(f.dispatched, a)
}

// fakeStore is an in-memory Store for the unit tests, including the
// (rule_id, event_id) dedup guard the real store enforces in SQL.
type fakeStore struct {
	monitors  []store.Monitor
	rules     map[int64][]store.Rule
	backfills map[int64]store.Backfill
	alerts    []store.Alert
	dedup     map[string]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rules:     map[int64][]store.Rule{},
		backfills: map[int64]store.Backfill{},
		dedup:     map[string]bool{},
	}
}

func (f *fakeStore) GetMonitor(_ context.Context, id int64) (*store.Monitor, error) {
	for i := range f.monitors {
		if f.monitors[i].ID == id {
			m := f.monitors[i]
			return &m, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListRules(_ context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error) {
	var out []store.Rule
	for _, r := range f.rules[monitorID] {
		if !enabledOnly || r.Enabled {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) CreateAlert(_ context.Context, a *store.Alert) (store.AlertOutcome, error) {
	key := fmt.Sprintf("%d/%s", a.RuleID, a.EventID)
	if f.dedup[key] {
		return store.AlertDuplicate, nil
	}
	f.dedup[key] = true
	a.ID = int64(len(f.alerts) + 1)
	a.CreatedAt = time.Unix(1_700_000_000, 0).UTC()
	f.alerts = append(f.alerts, *a)
	return store.AlertCreated, nil
}

func (f *fakeStore) GetBackfill(_ context.Context, monitorID int64) (store.Backfill, error) {
	b, ok := f.backfills[monitorID]
	if !ok {
		return store.Backfill{}, store.ErrNotFound
	}
	return b, nil
}

func (f *fakeStore) UpsertBackfill(_ context.Context, b *store.Backfill) error {
	f.backfills[b.MonitorID] = *b
	return nil
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newTestJob wires a Job over a fake source and store, with a monitor watching
// contractA and one event_emitted rule for "transfer".
func newTestJob(src poller.EventSource, st *fakeStore, d *fakeDispatcher) *Job {
	reg := rules.NewRegistry()
	ing := poller.NewIngestor(st, reg, d, discardLogger())
	return New(src, st, reg, ing, discardLogger())
}

// seedMonitor adds monitor 1 with a "transfer" event_emitted rule.
func seedMonitor(st *fakeStore) {
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{{
		ID: 1, MonitorID: 1, Type: rules.TypeEventEmitted,
		Params: json.RawMessage(`{"event_name": "transfer"}`), Enabled: true,
	}}
}

// transfer builds a decoded "transfer" event on contractA.
func transfer(id string, ledger uint32) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:             id,
		ContractID:     contractA,
		Ledger:         ledger,
		LedgerClosedAt: time.Unix(1_700_000_000, 0).UTC(),
		Topics:         []any{"transfer"},
		Value:          map[string]any{"i128": "1"},
	}
}

func twoPageSource() *fakeSource {
	return &fakeSource{
		tip: 100,
		pages: map[string]poller.FetchPage{
			"":   {Events: []*stellar.DecodedEvent{transfer("ev-1", 90), transfer("ev-2", 91)}, LatestLedger: 100, NextCursor: "c1"},
			"c1": {Events: []*stellar.DecodedEvent{transfer("ev-3", 92)}, LatestLedger: 100},
		},
	}
}

func TestOptionsValidate(t *testing.T) {
	assert.Error(t, Options{}.validate(), "a monitor id is required")
	assert.Error(t, Options{MonitorID: 1}.validate(), "a range is required")
	assert.NoError(t, Options{MonitorID: 1, FromLedger: 10}.validate())
	assert.NoError(t, Options{MonitorID: 1, Lookback: time.Hour}.validate())
}

func TestLookbackStart(t *testing.T) {
	assert.Equal(t, uint32(1000), lookbackStart(1000, 0), "no lookback means no window")
	// An hour at the ~5s ledger cadence is 720 ledgers.
	assert.Equal(t, uint32(280), lookbackStart(1000, time.Hour))
	assert.Equal(t, uint32(1), lookbackStart(10, 24*time.Hour), "never below ledger 1")
}

// TestRunClampsRequestedRangeToRetention is the contract for the retention
// window: a request older than the source can serve must be clamped, and the
// result must say how far back it actually went.
func TestRunClampsRequestedRangeToRetention(t *testing.T) {
	st := newFakeStore()
	seedMonitor(st)
	src := retainingSource{fakeSource: &fakeSource{tip: 1000, pages: map[string]poller.FetchPage{
		"": {LatestLedger: 1000},
	}}, oldest: 900}
	d := &fakeDispatcher{}

	res, err := newTestJob(src, st, d).Run(context.Background(), Options{
		MonitorID: 1, FromLedger: 100, ToLedger: 1000, Rate: time.Millisecond,
	})
	require.NoError(t, err)

	assert.True(t, res.Clamped)
	assert.Equal(t, uint32(100), res.RequestedFrom, "the requested start is reported unchanged")
	assert.Equal(t, uint32(900), res.FromLedger, "the run starts at the oldest retained ledger")
	assert.Equal(t, uint32(900), res.OldestLedger)
	require.Len(t, src.starts, 1)
	assert.Equal(t, uint32(900), src.starts[0], "the source is asked from the clamped ledger")
}

// TestRunMarksAlertsAndDoesNotDeliverByDefault is the core backfill contract:
// alerts are recorded as backfilled and stay off the wire unless asked.
func TestRunMarksAlertsAndDoesNotDeliverByDefault(t *testing.T) {
	st := newFakeStore()
	seedMonitor(st)
	d := &fakeDispatcher{}

	res, err := newTestJob(twoPageSource(), st, d).Run(context.Background(), Options{
		MonitorID: 1, FromLedger: 1, ToLedger: 100, Rate: time.Millisecond,
	})
	require.NoError(t, err)

	assert.Equal(t, 3, res.Alerts)
	assert.Zero(t, res.Dispatched)
	assert.Empty(t, d.dispatched, "backfill must not page anyone without -deliver")
	require.Len(t, st.alerts, 3)
	for _, a := range st.alerts {
		assert.True(t, a.Backfilled, "every backfilled alert must be marked")
	}
}

func TestRunDeliversWhenRequested(t *testing.T) {
	st := newFakeStore()
	seedMonitor(st)
	d := &fakeDispatcher{}

	res, err := newTestJob(twoPageSource(), st, d).Run(context.Background(), Options{
		MonitorID: 1, FromLedger: 1, ToLedger: 100, Deliver: true, Rate: time.Millisecond,
	})
	require.NoError(t, err)

	assert.Equal(t, 3, res.Dispatched)
	assert.Len(t, d.dispatched, 3, "delivery is explicit, and only then")
}

// TestRunResumesFromPersistedCursor covers the interruption path: a failed page
// leaves a resume point, and the next run continues from it instead of
// replaying the range from the start.
func TestRunResumesFromPersistedCursor(t *testing.T) {
	st := newFakeStore()
	seedMonitor(st)
	d := &fakeDispatcher{}
	opts := Options{MonitorID: 1, FromLedger: 1, ToLedger: 100, Rate: time.Millisecond}

	failing := twoPageSource()
	failing.failOn = 2 // serve the first page, then fail on the second fetch
	_, err := newTestJob(failing, st, d).Run(context.Background(), opts)
	require.Error(t, err)

	prog, err := st.GetBackfill(context.Background(), 1)
	require.NoError(t, err)
	assert.False(t, prog.Complete)
	assert.Equal(t, "c1", prog.Cursor, "the cursor of the last completed page is persisted")
	assert.Len(t, st.alerts, 2, "the first page's matches are already recorded")

	resumed := twoPageSource()
	res, err := newTestJob(resumed, st, d).Run(context.Background(), opts)
	require.NoError(t, err)

	assert.True(t, res.Resumed)
	require.NotEmpty(t, resumed.cursors)
	assert.Equal(t, "c1", resumed.cursors[0], "the run resumes at the persisted cursor")
	assert.Len(t, st.alerts, 3, "the resumed run only adds the remaining event")
	prog, err = st.GetBackfill(context.Background(), 1)
	require.NoError(t, err)
	assert.True(t, prog.Complete)
}

// TestRunStopsAtToLedger pins the bounded range: events past ToLedger are not
// replayed even though the source would happily keep returning them.
func TestRunStopsAtToLedger(t *testing.T) {
	st := newFakeStore()
	seedMonitor(st)
	d := &fakeDispatcher{}
	src := &fakeSource{tip: 100, pages: map[string]poller.FetchPage{
		"": {Events: []*stellar.DecodedEvent{
			transfer("ev-in", 50),
			transfer("ev-out", 60),
			transfer("ev-later", 70),
		}, LatestLedger: 100, NextCursor: "c1"},
		"c1": {Events: []*stellar.DecodedEvent{transfer("ev-never", 80)}, LatestLedger: 100},
	}}

	res, err := newTestJob(src, st, d).Run(context.Background(), Options{
		MonitorID: 1, FromLedger: 1, ToLedger: 55, Rate: time.Millisecond,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, res.Events, "only the in-range event is evaluated")
	assert.Equal(t, 1, res.Alerts)
	require.Len(t, src.cursors, 1, "the walk stops instead of paging past the range")
	assert.Equal(t, "ev-in", st.alerts[0].EventID)
}

func TestRunRejectsMonitorWithoutValidContracts(t *testing.T) {
	st := newFakeStore()
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{"not-a-contract"}, Enabled: true}}
	_, err := newTestJob(&fakeSource{tip: 10}, st, &fakeDispatcher{}).Run(context.Background(), Options{
		MonitorID: 1, FromLedger: 1,
	})
	require.Error(t, err)
}
