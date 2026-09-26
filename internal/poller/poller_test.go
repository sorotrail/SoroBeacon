package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// contractID builds a valid contract strkey from a seed byte; the poller
// drops invalid IDs, so test fixtures must be real strkeys.
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

// fakeRPC serves scripted getEvents responses and records requests.
type fakeRPC struct {
	latest    uint32
	responses []*stellar.GetEventsResult
	requests  []stellar.GetEventsRequest
}

func (f *fakeRPC) GetEvents(_ context.Context, req stellar.GetEventsRequest) (*stellar.GetEventsResult, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return &stellar.GetEventsResult{LatestLedger: f.latest}, nil
	}
	res := f.responses[0]
	f.responses = f.responses[1:]
	return res, nil
}

func (f *fakeRPC) GetLatestLedger(context.Context) (*stellar.LatestLedger, error) {
	return &stellar.LatestLedger{Sequence: f.latest}, nil
}

func (f *fakeRPC) GetNetwork(context.Context) (*stellar.Network, error) {
	return &stellar.Network{}, nil
}

func (f *fakeRPC) GetHealth(context.Context) (*stellar.Health, error) {
	return &stellar.Health{Status: "healthy"}, nil
}

// fakeStore implements poller.Store in memory, including the rule cooldown
// semantics the real store enforces in SQL. now is injectable so tests can
// advance the cooldown window without sleeping. The channel fields are
// only used by the tracing tests, which run deliveries through a real
// notify.Dispatcher; the plain poller tests never touch them.
type fakeStore struct {
	monitors []store.Monitor
	rules    map[int64][]store.Rule // monitor id -> rules
	state    store.IngestState
	alerts   []store.Alert
	dedup    map[string]bool // "ruleID/eventID"
	channels []store.Channel
	attached map[int64][]int64 // monitor id -> channel ids
	attempts []store.DeliveryAttempt

	now        func() time.Time
	lastFired  map[int64]time.Time // rule id -> last alert time
	suppressed map[int64]int64     // rule id -> matches dropped this window
	// ledgerHashes backs the reorg-detection half of the Store interface.
	ledgerHashes map[uint32]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rules:        map[int64][]store.Rule{},
		dedup:        map[string]bool{},
		attached:     map[int64][]int64{},
		now:          time.Now,
		lastFired:    map[int64]time.Time{},
		suppressed:   map[int64]int64{},
		ledgerHashes: map[uint32]string{},
	}
}

// ListChannelsForMonitor and RecordDeliveryAttempt satisfy the dispatcher's
// store interface so tracing tests can run real deliveries in memory.
func (f *fakeStore) ListChannelsForMonitor(_ context.Context, monitorID int64) ([]store.Channel, error) {
	var out []store.Channel
	for _, id := range f.attached[monitorID] {
		for _, ch := range f.channels {
			if ch.ID == id {
				out = append(out, ch)
			}
		}
	}
	return out, nil
}

func (f *fakeStore) RecordDeliveryAttempt(_ context.Context, d *store.DeliveryAttempt) error {
	d.ID = int64(len(f.attempts) + 1)
	f.attempts = append(f.attempts, *d)
	return nil
}

// ListChannels satisfies the digest half of the dispatcher's store
// interface; the poller tests never exercise digest flushing.
func (f *fakeStore) ListChannels(_ context.Context, enabledOnly bool) ([]store.Channel, error) {
	var out []store.Channel
	for _, ch := range f.channels {
		if !enabledOnly || ch.Enabled {
			out = append(out, ch)
		}
	}
	return out, nil
}

func (f *fakeStore) ListMonitors(_ context.Context, enabledOnly bool) ([]store.Monitor, error) {
	var out []store.Monitor
	for _, m := range f.monitors {
		if !enabledOnly || m.Enabled {
			out = append(out, m)
		}
	}
	return out, nil
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
		return store.AlertDuplicate, nil // replay, not a fresh match
	}
	// Mirror the real store: check the window before the dedup insert, and
	// count a suppressed match rather than writing an alert.
	if a.Cooldown > 0 {
		if last, ok := f.lastFired[a.RuleID]; ok && f.now().Before(last.Add(a.Cooldown)) {
			f.suppressed[a.RuleID]++
			return store.AlertSuppressed, nil
		}
	}
	f.dedup[key] = true
	a.ID = int64(len(f.alerts) + 1)
	if a.Cooldown > 0 {
		a.SuppressedSinceLast = f.suppressed[a.RuleID]
		f.suppressed[a.RuleID] = 0
		a.Payload = store.WithSuppressed(a.Payload, a.SuppressedSinceLast)
		f.lastFired[a.RuleID] = f.now()
	}
	f.alerts = append(f.alerts, *a)
	if !a.LedgerClosedAt.IsZero() {
		for i := range f.monitors {
			if f.monitors[i].ID != a.MonitorID {
				continue
			}
			t := a.LedgerClosedAt.UTC()
			if f.monitors[i].LastMatchedAt == nil || t.After(*f.monitors[i].LastMatchedAt) {
				f.monitors[i].LastMatchedAt = &t
			}
		}
	}
	return store.AlertCreated, nil
}

func (f *fakeStore) GetIngestState(context.Context) (store.IngestState, error) { return f.state, nil }
func (f *fakeStore) SetIngestState(_ context.Context, s store.IngestState) error {
	f.state = s
	return nil
}

// CreateAlertGroup creates or increments the alert group for key
// with windowStart. Returns the new count.
func (f *fakeStore) CreateAlertGroup(_ context.Context, key string, _ time.Time) (int64, error) {
	f.groupStates[key]++
	return f.groupStates[key], nil
}

// GroupAlerts creates or increments the alert group for key
// with windowStart and returns whether delivery should happen
// (first alert in the window) and the current count.
func (f *fakeStore) GroupAlerts(_ context.Context, key string, _ time.Time) (bool, int64, error) {
	count := f.groupStates[key] + 1
	f.groupStates[key] = count
	return count == 1, count, nil
}

// fakeDispatcher records dispatched alerts.
type fakeDispatcher struct {
	dispatched []notify.Alert
}

func (f *fakeDispatcher) Dispatch(_ context.Context, a notify.Alert) {
	f.dispatched = append(f.dispatched, a)
}

func transferEvent(id string, ledger uint32, amount string) stellar.Event {
	return stellar.Event{
		ID:             id,
		ContractID:     contractA,
		Ledger:         ledger,
		LedgerClosedAt: time.Unix(1_700_000_000, 0).UTC(),
		Type:           "contract",
		TopicJSON:      []json.RawMessage{json.RawMessage(`{"symbol": "transfer"}`)},
		ValueJSON:      json.RawMessage(fmt.Sprintf(`{"i128": %q}`, amount)),
	}
}

func newTestPoller(rpc *fakeRPC, st *fakeStore, d *fakeDispatcher) *Poller {
	src := NewRPCSource(rpc, stellar.DefaultDecoder{})
	return New(src, st, rules.NewRegistry(), d, 0, slog.New(slog.DiscardHandler))
}

func seedMonitor(st *fakeStore, ruleParams string) {
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{{ID: 1, MonitorID: 1, Type: rules.TypeEventEmitted,
		Params: json.RawMessage(ruleParams), Enabled: true}}
}

func TestPollColdStartUsesLatestLedger(t *testing.T) {
	rpc := &fakeRPC{latest: 5000}
	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	assert.Equal(t, uint32(5000), rpc.requests[0].StartLedger, "cold start begins at the tip")
	assert.Equal(t, uint32(5000), st.state.LastLedger, "checkpoint advances to latestLedger")
}

func TestPositionEmptyBeforeFirstPoll(t *testing.T) {
	p := newTestPoller(&fakeRPC{latest: 1}, newFakeStore(), &fakeDispatcher{})
	pos := p.Position()
	assert.False(t, pos.Ready())
	assert.Zero(t, pos.LastProcessedLedger)
	assert.Zero(t, pos.LatestChainLedger)
}

func TestPositionAfterSuccessfulPoll(t *testing.T) {
	rpc := &fakeRPC{latest: 5000}
	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	before := time.Now().UTC().Add(-time.Second)
	require.NoError(t, p.Poll(context.Background()))
	pos := p.Position()
	require.True(t, pos.Ready())
	assert.Equal(t, uint32(5000), pos.LastProcessedLedger)
	assert.Equal(t, uint32(5000), pos.LatestChainLedger)
	assert.Zero(t, pos.Lag())
	assert.False(t, pos.LastSuccessfulPoll.Before(before))
}

func TestPositionConcurrentReadDuringPoll(t *testing.T) {
	rpc := &fakeRPC{latest: 4200}
	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = p.Position()
		}
	}()
	require.NoError(t, p.Poll(context.Background()))
	wg.Wait()
	assert.True(t, p.Position().Ready())
}

func TestPollWarmStartResumesFromCheckpoint(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	assert.Equal(t, uint32(5501), rpc.requests[0].StartLedger, "warm start resumes after the checkpoint")
}

func TestPollMatchesAndDispatches(t *testing.T) {
	rpc := &fakeRPC{
		latest: 6000,
		responses: []*stellar.GetEventsResult{{
			Events: []stellar.Event{
				transferEvent("ev-1", 5990, "100"),
				{ID: "ev-2", ContractID: contractA, Ledger: 5991, Type: "contract",
					TopicJSON: []json.RawMessage{json.RawMessage(`{"symbol": "mint"}`)}},
			},
			LatestLedger: 6000,
		}},
	}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	d := &fakeDispatcher{}
	p := newTestPoller(rpc, st, d)

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, st.alerts, 1, "only the transfer event matches")
	assert.Equal(t, "ev-1", st.alerts[0].EventID)
	require.Len(t, d.dispatched, 1)
	assert.Equal(t, "m1", d.dispatched[0].MonitorName)
	assert.Equal(t, "transfer", d.dispatched[0].EventName)
	require.NotNil(t, st.monitors[0].LastMatchedAt)
	assert.True(t, st.monitors[0].LastMatchedAt.Equal(time.Unix(1_700_000_000, 0).UTC()),
		"last_matched_at must be the event ledger close time, not wall clock")
}

// TestPollPublishesCreatedAlertsToLiveStream pins the SSE publish point: an
// alert that is persisted is fanned out to live subscribers with the monitor's
// name and the stored payload, so a dashboard can render the row directly.
func TestPollPublishesCreatedAlertsToLiveStream(t *testing.T) {
	rpc := &fakeRPC{
		latest: 6000,
		responses: []*stellar.GetEventsResult{{
			Events:       []stellar.Event{transferEvent("ev-1", 5990, "100")},
			LatestLedger: 6000,
		}},
	}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)

	live := broadcast.New(4)
	sub := live.Subscribe(0)
	defer sub.Close()
	p := newTestPoller(rpc, st, &fakeDispatcher{}).WithPublisher(live)

	require.NoError(t, p.Poll(context.Background()))

	select {
	case got := <-sub.C:
		assert.Equal(t, st.alerts[0].ID, got.ID)
		assert.Equal(t, int64(1), got.MonitorID)
		assert.Equal(t, "m1", got.MonitorName)
		assert.Equal(t, "ev-1", got.EventID)
		assert.JSONEq(t, string(st.alerts[0].Payload), string(got.Payload))
	case <-time.After(2 * time.Second):
		t.Fatal("a created alert was not published to the live stream")
	}
}

// TestPollDoesNotPublishDedupedAlerts is the other half: a replayed event that
// the dedup guard rejects must not appear on the live stream, or a viewer would
// see the same alert twice.
func TestPollDoesNotPublishDedupedAlerts(t *testing.T) {
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	live := broadcast.New(4)
	sub := live.Subscribe(0)
	defer sub.Close()

	page := func() *fakeRPC {
		return &fakeRPC{latest: 6000, responses: []*stellar.GetEventsResult{{
			Events:       []stellar.Event{transferEvent("ev-dup", 5990, "1")},
			LatestLedger: 6000,
		}}}
	}
	// First poll creates and publishes; the replay is deduped and must not.
	require.NoError(t, newTestPoller(page(), st, &fakeDispatcher{}).WithPublisher(live).Poll(context.Background()))
	st.state.LastLedger = 5500
	require.NoError(t, newTestPoller(page(), st, &fakeDispatcher{}).WithPublisher(live).Poll(context.Background()))

	select {
	case got := <-sub.C:
		assert.Equal(t, "ev-dup", got.EventID)
	case <-time.After(2 * time.Second):
		t.Fatal("the created alert was not published")
	}
	select {
	case extra := <-sub.C:
		t.Fatalf("deduped alert was published again: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPollDedupsAcrossPolls(t *testing.T) {
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	d := &fakeDispatcher{}

	// Same event delivered twice (e.g. checkpoint replay after a crash).
	for i := 0; i < 2; i++ {
		rpc := &fakeRPC{latest: 6000, responses: []*stellar.GetEventsResult{{
			Events:       []stellar.Event{transferEvent("ev-dup", 5990, "1")},
			LatestLedger: 6000,
		}}}
		require.NoError(t, newTestPoller(rpc, st, d).Poll(context.Background()))
		st.state.LastLedger = 5500 // force a replay of the same window
	}

	assert.Len(t, st.alerts, 1, "dedup guard: one alert per (rule, event)")
	assert.Len(t, d.dispatched, 1, "duplicates must not be re-dispatched")
}

// burst feeds a poller one page of distinct transfer events, all matching the
// seeded rule. Reusing the same store across calls models successive polls.
func burst(t *testing.T, st *fakeStore, d *fakeDispatcher, ids ...string) {
	t.Helper()
	events := make([]stellar.Event, len(ids))
	for i, id := range ids {
		events[i] = transferEvent(id, 5990, "1")
	}
	rpc := &fakeRPC{latest: 6000, responses: []*stellar.GetEventsResult{{
		Events: events, LatestLedger: 6000,
	}}}
	require.NoError(t, newTestPoller(rpc, st, d).Poll(context.Background()))
}

// dispatchedSuppressed reads the suppressed-count an alert carried.
func dispatchedSuppressed(t *testing.T, a notify.Alert) int64 {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(a.Payload, &payload))
	n, _ := payload["suppressed_since_last"].(float64)
	return int64(n)
}

// TestPollCooldownSuppressesBurst is the core requirement: with a cooldown, a
// burst of matches collapses to exactly one alert.
func TestPollCooldownSuppressesBurst(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	st := newFakeStore()
	st.now = func() time.Time { return clock }
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer", "cooldown": "5m"}`)
	d := &fakeDispatcher{}

	burst(t, st, d, "ev-1", "ev-2", "ev-3", "ev-4", "ev-5")

	require.Len(t, st.alerts, 1, "a burst inside the cooldown yields exactly one alert")
	assert.Equal(t, "ev-1", st.alerts[0].EventID)
	require.Len(t, d.dispatched, 1, "suppressed matches must not be dispatched")
}

// TestPollCooldownWindowExpiresAndReportsSuppressed advances a fake clock past
// the window and checks the suppressed matches are surfaced on the next alert
// rather than silently lost.
func TestPollCooldownWindowExpiresAndReportsSuppressed(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	st := newFakeStore()
	st.now = func() time.Time { return clock }
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer", "cooldown": "5m"}`)
	d := &fakeDispatcher{}

	burst(t, st, d, "ev-1")
	burst(t, st, d, "ev-2", "ev-3", "ev-4") // suppressed
	require.Len(t, st.alerts, 1)

	// Still inside the window.
	clock = clock.Add(4 * time.Minute)
	burst(t, st, d, "ev-5")
	require.Len(t, st.alerts, 1, "4m is inside a 5m window")

	// Past the window: the next alert fires and reports the three suppressed
	// matches (ev-2, ev-3, ev-4; ev-5 landed while still inside).
	clock = clock.Add(2 * time.Minute)
	burst(t, st, d, "ev-6")
	require.Len(t, st.alerts, 2, "6m is past a 5m window")
	assert.Equal(t, "ev-6", st.alerts[1].EventID)

	require.Len(t, d.dispatched, 2)
	assert.EqualValues(t, 4, dispatchedSuppressed(t, d.dispatched[1]),
		"the alert after the window must report how many matches it swallowed")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(st.alerts[1].Payload, &payload))
	assert.EqualValues(t, 4, payload["suppressed_since_last"],
		"the stored payload carries the count too")
}

// TestPollWithoutCooldownAlertsEveryMatch pins the optional half: a rule with no
// cooldown behaves exactly as it did before the option existed.
func TestPollWithoutCooldownAlertsEveryMatch(t *testing.T) {
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	d := &fakeDispatcher{}

	burst(t, st, d, "ev-1", "ev-2", "ev-3")

	assert.Len(t, st.alerts, 3, "no cooldown means one alert per match")
	assert.Len(t, d.dispatched, 3)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(st.alerts[0].Payload, &payload))
	assert.NotContains(t, payload, "suppressed_since_last")
}

func TestPollFollowsCursor(t *testing.T) {
	// First page is exactly the page limit, so the poller must follow the
	// cursor; second page is short and ends the loop.
	page1 := make([]stellar.Event, stellar.DefaultEventsLimit)
	for i := range page1 {
		page1[i] = transferEvent(fmt.Sprintf("ev-a-%d", i), 5990, "1")
	}
	rpc := &fakeRPC{
		latest: 6000,
		responses: []*stellar.GetEventsResult{
			{Events: page1, LatestLedger: 6000, Cursor: "cursor-1"},
			{Events: []stellar.Event{transferEvent("ev-b", 5995, "1")}, LatestLedger: 6001},
		},
	}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 2)
	assert.Equal(t, "cursor-1", rpc.requests[1].Pagination.Cursor)
	assert.Zero(t, rpc.requests[1].StartLedger, "cursor requests must omit startLedger")
	assert.Len(t, st.alerts, stellar.DefaultEventsLimit+1)
}

func TestPollBatchesContractsAcrossFilters(t *testing.T) {
	// 26 contracts -> 6 filters of <=5 contracts -> 2 requests (cap 5
	// filters per request).
	var contracts []string
	for i := 0; i < 26; i++ {
		contracts = append(contracts, contractID(byte(i)))
	}
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: contracts, Enabled: true}}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 2)
	assert.Len(t, rpc.requests[0].Filters, 5)
	assert.Len(t, rpc.requests[1].Filters, 1)
	for _, req := range rpc.requests {
		for _, f := range req.Filters {
			assert.LessOrEqual(t, len(f.ContractIDs), stellar.MaxContractIDsPerFilter)
		}
	}
}

func TestPollNoMonitorsIsNoop(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))
	assert.Empty(t, rpc.requests, "no enabled monitors means no RPC calls")
}

func TestPollSkipsInvalidContractIDs(t *testing.T) {
	// One bad contract ID must be dropped, not sent: the RPC would reject
	// the whole request and stall ingestion for every monitor.
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.state.LastLedger = 5500
	st.monitors = []store.Monitor{{ID: 1, Name: "m1",
		ContractIDs: []string{"not-a-contract", contractA}, Enabled: true}}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 1)
	assert.Equal(t, []string{contractA}, rpc.requests[0].Filters[0].ContractIDs)
}

func TestPollIgnoresEventsFromUnwatchedContracts(t *testing.T) {
	rpc := &fakeRPC{latest: 6000, responses: []*stellar.GetEventsResult{{
		Events: []stellar.Event{{
			ID: "ev-x", ContractID: "COTHER", Ledger: 5990, Type: "contract",
			TopicJSON: []json.RawMessage{json.RawMessage(`{"symbol": "transfer"}`)},
		}},
		LatestLedger: 6000,
	}}}
	st := newFakeStore()
	st.state.LastLedger = 5500
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))
	assert.Empty(t, st.alerts)
}

// TestPollDerivesTopicFiltersFromNamedRules pins the optimisation: when every
// enabled rule on a contract names an event, the poller narrows the getEvents
// filter so the node does not stream events we would discard.
func TestPollDerivesTopicFiltersFromNamedRules(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	seedMonitor(st, `{"event_name": "transfer"}`)
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 1)
	f := rpc.requests[0].Filters[0]
	assert.Equal(t, []string{contractA}, f.ContractIDs)
	// "transfer" encodes to the ScVal the RPC docs use, followed by a
	// trailing wildcard so events with more topics still match.
	assert.Equal(t, [][]string{{"AAAADwAAAAh0cmFuc2Zlcg==", "**"}}, f.Topics)
}

// TestPollLeavesContractUnfilteredWhenAnyRuleIsUnnamed is the safety half: a
// single rule that matches unnamed events must keep the contract unfiltered,
// because a narrowed filter would silently drop events that rule would match.
func TestPollLeavesContractUnfilteredWhenAnyRuleIsUnnamed(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{
		{ID: 1, MonitorID: 1, Type: rules.TypeEventEmitted, Params: json.RawMessage(`{"event_name": "transfer"}`), Enabled: true},
		{ID: 2, MonitorID: 1, Type: rules.TypeValueThreshold, Params: json.RawMessage(`{"comparison": "gt", "threshold": 1}`), Enabled: true},
	}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 1)
	assert.Empty(t, rpc.requests[0].Filters[0].Topics, "a rule with no event_name forces an unfiltered request")
}

// TestPollFallsBackUnfilteredOnTopicCapOverflow checks the RPC's cap: more
// distinct event names than a filter can express must fall back to unfiltered
// rather than truncating, which would drop events.
func TestPollFallsBackUnfilteredOnTopicCapOverflow(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	for i, name := range []string{"a", "b", "c", "d", "e", "f"} {
		st.rules[1] = append(st.rules[1], store.Rule{
			ID: int64(i + 1), MonitorID: 1, Type: rules.TypeEventEmitted,
			Params: json.RawMessage(fmt.Sprintf(`{"event_name": %q}`, name)), Enabled: true,
		})
	}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 1)
	assert.Empty(t, rpc.requests[0].Filters[0].Topics, "six names exceed the five-filter cap")
}

// TestPollGroupsContractsByTopicFilter verifies contracts whose rules name
// different events get their own filter, since a filter's topics apply to
// every contract ID it carries.
func TestPollGroupsContractsByTopicFilter(t *testing.T) {
	contractB := contractID(0xB2)
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.monitors = []store.Monitor{
		{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true},
		{ID: 2, Name: "m2", ContractIDs: []string{contractB}, Enabled: true},
	}
	st.rules[1] = []store.Rule{{ID: 1, MonitorID: 1, Type: rules.TypeEventEmitted,
		Params: json.RawMessage(`{"event_name": "transfer"}`), Enabled: true}}
	st.rules[2] = []store.Rule{{ID: 2, MonitorID: 2, Type: rules.TypeEventEmitted,
		Params: json.RawMessage(`{"event_name": "mint"}`), Enabled: true}}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 2)
	topicsByContract := map[string][][]string{}
	for _, f := range rpc.requests[0].Filters {
		for _, id := range f.ContractIDs {
			topicsByContract[id] = f.Topics
		}
	}
	assert.Equal(t, [][]string{{"AAAADwAAAAh0cmFuc2Zlcg==", "**"}}, topicsByContract[contractA])
	assert.Equal(t, [][]string{{"AAAADwAAAARtaW50", "**"}}, topicsByContract[contractB])
}

// TestPollLeavesWildcardTokenRulesUnfiltered covers a rule type whose "*"
// event genuinely matches several events: it can still be named, so the filter
// is built from the concrete SEP-41 names rather than dropped.
func TestPollNamesWildcardTokenEvents(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{{ID: 1, MonitorID: 1, Type: rules.TypeTokenEvent,
		Params: json.RawMessage(`{"event": "*"}`), Enabled: true}}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 1)
	assert.Len(t, rpc.requests[0].Filters[0].Topics, 5, "the five SEP-41 events all become topic filters")
}

// TestPollFrequencyRuleFiresOncePerCrossing is the end-to-end shape of the
// frequency rule: a burst that crosses the threshold yields one alert, stored
// under the rule's synthetic window-start id so a replay is deduplicated too.
func TestPollFrequencyRuleFiresOncePerCrossing(t *testing.T) {
	st := newFakeStore()
	st.state.LastLedger = 5500
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{{ID: 1, MonitorID: 1, Type: rules.TypeFrequencyThreshold,
		Params: json.RawMessage(`{"event_name": "transfer", "count": 3, "window": "5m"}`), Enabled: true}}
	d := &fakeDispatcher{}

	burst(t, st, d, "ev-1", "ev-2", "ev-3", "ev-4", "ev-5")

	require.Len(t, st.alerts, 1, "the crossing yields exactly one alert")
	require.Len(t, d.dispatched, 1)
	assert.Contains(t, st.alerts[0].EventID, "frequency:",
		"the alert is keyed by the synthetic episode id, not the crossing event")
	assert.Equal(t, st.alerts[0].EventID, d.dispatched[0].EventID)
}

// TestPollIgnoresDisabledRulesWhenDerivingTopics makes sure a disabled rule
// cannot force an unfiltered request for a contract whose enabled rules are
// all named.
func TestPollIgnoresDisabledRulesWhenDerivingTopics(t *testing.T) {
	rpc := &fakeRPC{latest: 6000}
	st := newFakeStore()
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{
		{ID: 1, MonitorID: 1, Type: rules.TypeEventEmitted, Params: json.RawMessage(`{"event_name": "transfer"}`), Enabled: true},
		{ID: 2, MonitorID: 1, Type: rules.TypeEventEmitted, Params: json.RawMessage(`{}`), Enabled: false},
	}
	p := newTestPoller(rpc, st, &fakeDispatcher{})

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, rpc.requests, 1)
	require.Len(t, rpc.requests[0].Filters, 1)
	assert.Equal(t, [][]string{{"AAAADwAAAAh0cmFuc2Zlcg==", "**"}}, rpc.requests[0].Filters[0].Topics)
}
