package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

var contractB = contractID(0xB2)

// netSource is an EventSource for the supervisor tests. It serves one transfer
// per watched contract at its own chain tip, and can be made to fail or panic
// on command — which is the point. The supervisor's promise is that one
// network's problem stops with that network, and the only way to show that is
// a source that misbehaves while its neighbour behaves.
type netSource struct {
	// tip is the height this chain reports; a test raises it to advance the
	// chain between cycles.
	tip atomic.Uint32
	// failing makes every call return an error, as an unreachable node does.
	failing atomic.Bool
	// panicking makes the next cycle panic instead of returning.
	panicking atomic.Bool
	// drift is how many ledgers the chain advances while one cycle pages, so
	// a test can give a network a specific, non-zero lag.
	drift uint32

	mu          sync.Mutex
	cycles      int64
	resumed     atomic.Int64
	watched     []string
	checkpoints []uint32
}

func (s *netSource) LatestLedger(context.Context) (uint32, error) {
	if s.failing.Load() {
		return 0, errors.New("rpc unreachable")
	}
	return s.tip.Load(), nil
}

func (s *netSource) FetchEvents(_ context.Context, startLedger uint32, watch []Watch, cursor string, _ int) (FetchPage, error) {
	if s.panicking.Load() && cursor == "" {
		s.panicking.Store(false)
		s.resumed.Add(1)
		panic("test panic from the event source")
	}
	if s.failing.Load() {
		return FetchPage{}, errors.New("rpc unreachable")
	}
	if cursor == "cont" {
		// The continuation page sees the chain further along, which is what
		// makes a cycle's lag non-zero: the checkpoint is the first page's tip.
		return FetchPage{LatestLedger: s.tip.Load() + s.drift}, nil
	}

	s.mu.Lock()
	s.cycles++
	s.watched = nil
	for _, w := range watch {
		s.watched = append(s.watched, w.ContractID)
	}
	s.checkpoints = append(s.checkpoints, startLedger)
	s.mu.Unlock()

	tip := s.tip.Load()
	page := FetchPage{LatestLedger: tip}
	if startLedger > tip || len(watch) == 0 {
		// Nothing new since the last checkpoint. Returning no events is the
		// honest answer; the poller still advances the cursor to the tip.
		if s.drift > 0 {
			page.NextCursor = "cont"
		}
		return page, nil
	}
	for _, w := range watch {
		page.Events = append(page.Events, transferAt(w.ContractID, tip))
	}
	if s.drift > 0 {
		page.NextCursor = "cont"
	}
	return page, nil
}

// snapshot reports how many cycles this source has served and which contracts
// the last cycle asked for.
func (s *netSource) snapshot() (cycles int64, watched []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cycles, append([]string(nil), s.watched...)
}

// transferAt builds one decoded transfer event on a contract at a ledger. The
// source hands the poller already-decoded events, so the fake constructs them
// directly rather than going through the XDR path.
func transferAt(contract string, ledger uint32) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:             fmt.Sprintf("%s-%d", contract, ledger),
		ContractID:     contract,
		Ledger:         ledger,
		LedgerClosedAt: time.Unix(1_700_000_000, 0).UTC(),
		Topics:         []any{"transfer"},
		Value:          map[string]any{"i128": "100"},
	}
}

// addNetworkMonitor seeds one network's monitor and its event rule, returning
// the poller's source-side contract.
func addNetworkMonitor(st *fakeStore, id int64, network, contract string) {
	st.monitors = append(st.monitors, store.Monitor{
		ID:          id,
		Name:        network + "-monitor",
		Network:     network,
		ContractIDs: []string{contract},
		Enabled:     true,
	})
	st.rules[id] = []store.Rule{{
		ID:        id,
		MonitorID: id,
		Type:      rules.TypeEventEmitted,
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
	}}
}

func newNetPoller(src *netSource, st *fakeStore, d *fakeDispatcher, network string, m *metrics.Metrics) *Poller {
	return New(src, st, rules.NewRegistry(), d, 2*time.Millisecond, slog.New(slog.DiscardHandler)).
		WithNetwork(network).
		WithMetrics(m)
}

// eventually polls cond until it holds or the deadline passes, failing the test
// with a snapshot of where each network got to.
func eventually(t *testing.T, sup *Supervisor, want string, cond func() bool) {
	t.Helper()
	ok := false
	for i := 0; i < 400 && !ok; i++ {
		if cond() {
			ok = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("condition failed: %s\npositions: %+v", want, sup.Positions())
	}
}

func TestSupervisorPollsTwoNetworksWithIndependentCursors(t *testing.T) {
	// The chains are at very different heights and share one store. If the
	// cursor were single-row, the two pollers would fight over it and one
	// network's checkpoint would end up at the other chain's height — which
	// means silently skipping or replaying events, depending on the order.
	st := newFakeStore()
	addNetworkMonitor(st, 1, "testnet", contractA)
	addNetworkMonitor(st, 2, "mainnet", contractB)
	m := metrics.New()

	testnet, mainnet := &netSource{drift: 5}, &netSource{drift: 40}
	testnet.tip.Store(100)
	mainnet.tip.Store(5_000)
	// One dispatcher per network: the poller hands each alert to its own
	// dispatcher, so sharing one here would make the test racy rather than
	// the code under test.
	units := []Unit{
		{Network: "testnet", Poller: newNetPoller(testnet, st, &fakeDispatcher{}, "testnet", m.WithNetwork("testnet"))},
		{Network: "mainnet", Poller: newNetPoller(mainnet, st, &fakeDispatcher{}, "mainnet", m.WithNetwork("mainnet"))},
	}
	sup := NewSupervisor(slog.New(slog.DiscardHandler), units...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	eventually(t, sup, "both networks reach their own tip", func() bool {
		a, b := units[0].Poller.Position(), units[1].Poller.Position()
		return a.LastProcessedLedger == 100 && b.LastProcessedLedger == 5_000
	})
	cancel()

	// One store, two rows: each network advanced only its own, and the legacy
	// single-row checkpoint stayed empty because no poller was unscoped.
	assert.Equal(t, uint32(100), st.namedState["testnet"].LastLedger)
	assert.Equal(t, uint32(5_000), st.namedState["mainnet"].LastLedger)
	assert.Zero(t, st.state.LastLedger, "a network-scoped poller must not write the legacy ingest row")

	// Each poller saw only its own network's monitor, so it only ever asked
	// for its own contract.
	_, watched := testnet.snapshot()
	assert.Equal(t, []string{contractA}, watched)
	_, watched = mainnet.snapshot()
	assert.Equal(t, []string{contractB}, watched)

	// Both networks alerted, and each alert carries the chain it came from —
	// the thing that makes a mainnet/testnet pair readable in one feed.
	require.Len(t, st.alerts, 2)
	networks := map[string]bool{}
	for _, a := range st.alerts {
		networks[a.Network] = true
	}
	assert.Equal(t, map[string]bool{"testnet": true, "mainnet": true}, networks)

	// Every ingest metric carries the network, so the two are distinguishable
	// in one scrape rather than summed into one meaningless number.
	body := scrape(t, m)
	assert.Contains(t, body, `sorobeacon_poll_lag_ledgers{network="testnet"}`)
	assert.Contains(t, body, `sorobeacon_poll_lag_ledgers{network="mainnet"}`)
	assert.NotContains(t, body, `sorobeacon_poll_lag_ledgers{network=""}`)
}

func TestSupervisorIsolatesAFailingNetwork(t *testing.T) {
	// testnet's node is unreachable from the first cycle. The requirement is
	// not that SoroBeacon notices — it is that mainnet keeps ingesting,
	// because a missed alert on a healthy chain is the failure nobody sees.
	st := newFakeStore()
	addNetworkMonitor(st, 1, "testnet", contractA)
	addNetworkMonitor(st, 2, "mainnet", contractB)

	broken := &netSource{}
	broken.tip.Store(100)
	broken.failing.Store(true)
	working := &netSource{}
	working.tip.Store(200)
	units := []Unit{
		{Network: "testnet", Poller: newNetPoller(broken, st, &fakeDispatcher{}, "testnet", nil)},
		{Network: "mainnet", Poller: newNetPoller(working, st, &fakeDispatcher{}, "mainnet", nil)},
	}
	sup := NewSupervisor(slog.New(slog.DiscardHandler), units...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	eventually(t, sup, "the healthy network ingests while the other fails", func() bool {
		return units[1].Poller.Position().LastProcessedLedger == 200
	})
	// ...and keeps polling: backoff is per poller, so the failing one's
	// exponential retry cannot throttle this one.
	first, _ := working.snapshot()
	for i := 0; i < 20; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	second, _ := working.snapshot()
	assert.Greater(t, second, first, "the healthy network's cycle count must keep climbing")

	assert.False(t, units[0].Poller.Position().Ready(), "the failing network polled successfully, which it should not have")
	assert.Zero(t, st.namedState["testnet"].LastLedger, "a chain that never answered must not advance")

	// The aggregate is the union of the bad news: one network without a
	// successful poll means the instance is not caught up, so /readyz cannot
	// pass on the healthy chain's numbers alone.
	assert.False(t, sup.Position().Ready())
	cancel()
}

func TestSupervisorRestartsAPanickedLoop(t *testing.T) {
	// A panic in one network's cycle used to take the process down, which
	// stopped every network at once. Recovered, it costs that one chain one
	// cycle — and shows up in its own metric instead of only in the log.
	st := newFakeStore()
	addNetworkMonitor(st, 1, "testnet", contractA)
	addNetworkMonitor(st, 2, "mainnet", contractB)
	m := metrics.New()
	logged := &syncBuffer{}

	crashy := &netSource{}
	crashy.tip.Store(70)
	crashy.panicking.Store(true)
	healthy := &netSource{}
	healthy.tip.Store(70)
	units := []Unit{
		{Network: "testnet", Poller: newNetPoller(crashy, st, &fakeDispatcher{}, "testnet", m.WithNetwork("testnet"))},
		{Network: "mainnet", Poller: newNetPoller(healthy, st, &fakeDispatcher{}, "mainnet", m.WithNetwork("mainnet"))},
	}
	sup := NewSupervisor(slog.New(slog.NewTextHandler(logged, nil)), units...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	eventually(t, sup, "the panicking loop restarts and finishes the cycle", func() bool {
		return units[0].Poller.Position().LastProcessedLedger == 70
	})
	assert.Equal(t, int64(1), crashy.resumed.Load(), "exactly one cycle panicked")
	assert.Contains(t, logged.String(), "poller panicked")
	assert.Contains(t, logged.String(), `network=testnet`)

	// Counted per network, so an operator can alert on the chain that is
	// crashing rather than on a process-wide total. A counter only appears
	// once observed, so mainnet's absence is what "zero" looks like here.
	body := scrape(t, m)
	assert.Contains(t, body, `sorobeacon_poll_panics_total{network="testnet"} 1`)
	assert.NotContains(t, body, `sorobeacon_poll_panics_total{network="mainnet"}`)

	// The neighbour is untouched: same tip, no panic attributed to it, and it
	// ingested normally.
	assert.Equal(t, uint32(70), units[1].Poller.Position().LastProcessedLedger)
	assert.Equal(t, uint32(70), st.namedState["mainnet"].LastLedger)
	cancel()
}

func TestSupervisorPositionsAndAggregate(t *testing.T) {
	st := newFakeStore()
	addNetworkMonitor(st, 1, "testnet", contractA)
	addNetworkMonitor(st, 2, "mainnet", contractB)

	testnet, mainnet := &netSource{drift: 5}, &netSource{drift: 40}
	testnet.tip.Store(100)
	mainnet.tip.Store(100)
	units := []Unit{
		{Network: "testnet", Poller: newNetPoller(testnet, st, &fakeDispatcher{}, "testnet", nil)},
		{Network: "mainnet", Poller: newNetPoller(mainnet, st, &fakeDispatcher{}, "mainnet", nil)},
	}
	sup := NewSupervisor(slog.New(slog.DiscardHandler), units...)
	ctx := context.Background()

	// Before any poll, the aggregate is not ready — which is what keeps
	// /health from publishing a zero-lag position that reads as caught up.
	assert.False(t, sup.Position().Ready())
	assert.Len(t, sup.Positions(), 2)

	require.NoError(t, units[0].Poller.Poll(ctx))
	assert.False(t, sup.Position().Ready(), "one network polling is not the instance being ready")

	require.NoError(t, units[1].Poller.Poll(ctx))
	positions := sup.Positions()
	assert.Equal(t, "testnet", positions[0].Network, "positions stay in configured, primary-first order")
	assert.Equal(t, "mainnet", positions[1].Network)
	assert.Equal(t, int64(5), positions[0].Lag())
	assert.Equal(t, int64(40), positions[1].Lag())

	// The aggregate is the worst chain, whichever network it is: mainnet is
	// second in the list here, and its lag is the number that gets reported.
	agg := sup.Position()
	assert.Equal(t, "aggregate", agg.Network)
	assert.Equal(t, int64(40), agg.Lag())
	assert.Equal(t, uint32(100), agg.LastProcessedLedger)
	assert.Equal(t, uint32(140), agg.LatestChainLedger)
}

func TestSupervisorStatusesReportPerNetwork(t *testing.T) {
	st := newFakeStore()
	addNetworkMonitor(st, 1, "testnet", contractA)
	addNetworkMonitor(st, 2, "mainnet", contractB)

	testnet := &netSource{}
	testnet.tip.Store(100)
	mainnet := &netSource{}
	mainnet.tip.Store(500)
	mainnet.failing.Store(true)
	units := []Unit{
		{Network: "testnet", Poller: newNetPoller(testnet, st, &fakeDispatcher{}, "testnet", nil)},
		{Network: "mainnet", Poller: newNetPoller(mainnet, st, &fakeDispatcher{}, "mainnet", nil)},
	}
	sup := NewSupervisor(slog.New(slog.DiscardHandler), units...)
	ctx := context.Background()

	// Nothing has polled yet: both networks are still listed, with a live
	// chain's tip and the whole height as lag.
	statuses := sup.Statuses(ctx)
	require.Len(t, statuses, 2)
	assert.Equal(t, "testnet", statuses[0].Network)
	assert.Equal(t, "ok", statuses[0].Source)
	assert.Empty(t, statuses[0].LastPollAt, "no successful poll yet")
	assert.Equal(t, int64(100), statuses[0].LedgerLag)

	require.NoError(t, units[0].Poller.Poll(ctx))
	statuses = sup.Statuses(ctx)
	assert.Equal(t, uint32(100), statuses[0].LastProcessedLedger)
	assert.Equal(t, int64(0), statuses[0].LedgerLag)
	assert.NotEmpty(t, statuses[0].LastPollAt)

	// A source that cannot answer is named, and its lag stays the poller's
	// own last known number rather than a fabricated zero.
	assert.Equal(t, "rpc unreachable", statuses[1].Source)
	assert.Zero(t, statuses[1].LatestChainLedger)
	assert.Zero(t, statuses[1].LedgerLag)

	// With mainnet's poll recorded, its lag is measured against the stale tip
	// the poller last saw, not 0 - 0.
	mainnet.failing.Store(false)
	require.NoError(t, units[1].Poller.Poll(ctx))
	mainnet.failing.Store(true)
	statuses = sup.Statuses(ctx)
	assert.Equal(t, uint32(500), statuses[1].LastProcessedLedger)
	assert.Equal(t, int64(0), statuses[1].LedgerLag)
}

// scrape renders the metric set the way /metrics does, so a test asserts on
// the labels an operator actually queries.
func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

// syncBuffer is a concurrency-safe log sink: the supervisor writes from one
// goroutine per network, so a plain bytes.Buffer would race.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// TestSupervisorWarnsAboutMonitorsNoNetworkPolls covers the silent failure a
// multi-network rollout makes easiest: a monitor labelled with a chain nobody
// polls (or left unlabelled because startup's labelling missed it) never
// matches anything, and the dashboard shows it as simply quiet.
func TestSupervisorWarnsAboutMonitorsNoNetworkPolls(t *testing.T) {
	st := newFakeStore()
	addNetworkMonitor(st, 1, "testnet", contractA)
	addNetworkMonitor(st, 2, "mainnet", contractB)
	// Watched by nobody: one chain outside the list, one chain named by
	// nobody at all.
	addNetworkMonitor(st, 3, "futurenet", "CFFF")
	addNetworkMonitor(st, 4, "", "CGGG")

	logged := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logged, nil))
	testnet, mainnet := &netSource{drift: 1}, &netSource{drift: 1}
	testnet.tip.Store(100)
	mainnet.tip.Store(100)
	newLogged := func(src *netSource, network string) *Poller {
		return New(src, st, rules.NewRegistry(), &fakeDispatcher{}, 2*time.Millisecond, log).
			WithNetwork(network)
	}
	units := []Unit{
		{Network: "testnet", Poller: newLogged(testnet, "testnet")},
		{Network: "mainnet", Poller: newLogged(mainnet, "mainnet")},
	}
	sup := NewSupervisor(log, units...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	eventually(t, sup, "both units poll", func() bool {
		return len(sup.Positions()) == 2 && !sup.Positions()[0].LastSuccessfulPoll.IsZero() &&
			!sup.Positions()[1].LastSuccessfulPoll.IsZero()
	})

	out := logged.String()
	require.Contains(t, out, "monitors belong to no polled network")
	assert.Contains(t, out, "3=futurenet")
	assert.Contains(t, out, "4=<none>")
	assert.NotContains(t, out, "1=testnet", "a monitor its own poller watches is not an orphan")
	assert.NotContains(t, out, "2=mainnet", "a sibling unit's monitor is not an orphan")
	// The 2ms interval would otherwise write this line thousands of times.
	assert.Equal(t, 1, strings.Count(out, "monitors belong to no polled network"),
		"the warning must latch: %s", out)
}

// TestPollerWithoutSupervisorStaysSilent pins the single-network case: a bare
// Poller never claims to know the instance's other networks, so it must not
// warn about monitors it simply does not watch.
func TestPollerWithoutSupervisorStaysSilent(t *testing.T) {
	st := newFakeStore()
	addNetworkMonitor(st, 1, "testnet", contractA)
	addNetworkMonitor(st, 2, "mainnet", contractB)

	logged := &syncBuffer{}
	src := &netSource{drift: 1}
	src.tip.Store(10)
	p := newNetPoller(src, st, &fakeDispatcher{}, "testnet", nil)
	p.log = slog.New(slog.NewTextHandler(logged, nil))

	require.NoError(t, p.Poll(context.Background()))
	assert.NotContains(t, logged.String(), "no polled network")
}
