package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func (f *fakeRPC) GetHealth(context.Context) (*stellar.Health, error) {
	return &stellar.Health{Status: "healthy"}, nil
}

// fakeStore implements poller.Store in memory.
type fakeStore struct {
	monitors []store.Monitor
	rules    map[int64][]store.Rule // monitor id -> rules
	state    store.IngestState
	alerts   []store.Alert
	dedup    map[string]bool // "ruleID/eventID"
}

func newFakeStore() *fakeStore {
	return &fakeStore{rules: map[int64][]store.Rule{}, dedup: map[string]bool{}}
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

func (f *fakeStore) CreateAlert(_ context.Context, a *store.Alert) (bool, error) {
	key := fmt.Sprintf("%d/%s", a.RuleID, a.EventID)
	if f.dedup[key] {
		return false, nil
	}
	f.dedup[key] = true
	a.ID = int64(len(f.alerts) + 1)
	f.alerts = append(f.alerts, *a)
	return true, nil
}

func (f *fakeStore) GetIngestState(context.Context) (store.IngestState, error) { return f.state, nil }
func (f *fakeStore) SetIngestState(_ context.Context, s store.IngestState) error {
	f.state = s
	return nil
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
		ID:         id,
		ContractID: contractA,
		Ledger:     ledger,
		Type:       "contract",
		TopicJSON:  []json.RawMessage{json.RawMessage(`{"symbol": "transfer"}`)},
		ValueJSON:  json.RawMessage(fmt.Sprintf(`{"i128": %q}`, amount)),
	}
}

func newTestPoller(rpc *fakeRPC, st *fakeStore, d *fakeDispatcher) *Poller {
	return New(rpc, stellar.DefaultDecoder{}, st, rules.NewRegistry(), d, 0,
		slog.New(slog.DiscardHandler))
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
