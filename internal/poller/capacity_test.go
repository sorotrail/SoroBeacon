package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// capacityScenario defines a fixed, repeatable workload for measuring poller
// evaluation throughput across monitors, rules per monitor, events per poll,
// and match rate.
type capacityScenario struct {
	name            string
	monitors        int
	rulesPerMonitor int
	eventsPerPoll   int
	pageSize        int
	matchEveryN     int // 0 means 0% match; 1 means 100% match; 10 means 10% match
	ledgerSpan      uint32
}

// buildCapacityFixtures seeds a fakeStore and scripted GetEventsResult pages
// for the given scenario. All monitors watch contractA so every polled event
// traverses the full monitor x rule evaluation matrix.
func buildCapacityFixtures(sc capacityScenario, iter int) (*fakeStore, []*stellar.GetEventsResult) {
	st := newFakeStore()
	st.state = store.IngestState{LastLedger: 5000}

	var ruleID int64 = 1
	for mIdx := 1; mIdx <= sc.monitors; mIdx++ {
		mID := int64(mIdx)
		st.monitors = append(st.monitors, store.Monitor{
			ID:          mID,
			Name:        fmt.Sprintf("cap-mon-%d", mID),
			ContractIDs: []string{contractA},
			Enabled:     true,
		})
		for rIdx := 0; rIdx < sc.rulesPerMonitor; rIdx++ {
			st.rules[mID] = append(st.rules[mID], store.Rule{
				ID:        ruleID,
				MonitorID: mID,
				Type:      rules.TypeValueThreshold,
				Params:    json.RawMessage(`{"event_name":"transfer","comparison":"gt","threshold":100}`),
				Enabled:   true,
			})
			ruleID++
		}
	}

	pageSize := sc.pageSize
	if pageSize <= 0 {
		pageSize = sc.eventsPerPoll
	}
	if pageSize <= 0 {
		pageSize = 1
	}

	latestLedger := uint32(5000) + sc.ledgerSpan
	if sc.ledgerSpan == 0 {
		latestLedger = 5001
	}

	var pages []*stellar.GetEventsResult
	var currentEvents []stellar.Event
	for eIdx := 0; eIdx < sc.eventsPerPoll; eIdx++ {
		amount := "50" // non-matching (<= 100)
		if sc.matchEveryN > 0 && (eIdx%sc.matchEveryN == 0) {
			amount = "500" // matching (> 100)
		}
		evID := fmt.Sprintf("cap-%d-%06d", iter, eIdx)
		ledger := uint32(5001)
		if sc.ledgerSpan > 1 {
			ledger = 5001 + uint32(eIdx)%sc.ledgerSpan
		}
		currentEvents = append(currentEvents, transferEvent(evID, ledger, amount))
		if len(currentEvents) == pageSize && eIdx < sc.eventsPerPoll-1 {
			pages = append(pages, &stellar.GetEventsResult{
				Events:       currentEvents,
				LatestLedger: latestLedger,
				Cursor:       fmt.Sprintf("cursor-%d-%d", iter, eIdx),
			})
			currentEvents = nil
		}
	}
	pages = append(pages, &stellar.GetEventsResult{
		Events:       currentEvents,
		LatestLedger: latestLedger,
	})

	return st, pages
}

// TestCapacityModelHarnessesAndScaling verifies that the capacity benchmark
// workloads (stage breakdown, scaling dimensions, and steady-state vs catch-up)
// execute deterministically and produce the expected evaluation and alert counts.
func TestCapacityModelHarnessesAndScaling(t *testing.T) {
	ctx := context.Background()

	steady := capacityScenario{
		name:            "steady_state",
		monitors:        5,
		rulesPerMonitor: 4,
		eventsPerPoll:   20,
		pageSize:        20,
		matchEveryN:     10, // 2 matching events * 5 monitors * 4 rules = 40 alerts
		ledgerSpan:      1,
	}
	stSteady, pagesSteady := buildCapacityFixtures(steady, 0)
	rpcSteady := &fakeRPC{latest: 5001, responses: pagesSteady}
	dispSteady := &fakeDispatcher{}
	pSteady := newTestPoller(rpcSteady, stSteady, dispSteady)

	require.NoError(t, pSteady.Poll(ctx))
	assert.Equal(t, 20, pSteady.scanned)
	assert.Equal(t, 20*5*4, pSteady.evaluations)
	assert.Equal(t, 40, pSteady.matched)
	assert.Len(t, stSteady.alerts, 40)
	assert.Len(t, dispSteady.dispatched, 40)

	catchUp := capacityScenario{
		name:            "catch_up",
		monitors:        5,
		rulesPerMonitor: 4,
		eventsPerPoll:   300,
		pageSize:        stellar.DefaultEventsLimit, // 100 events/page -> 3 paginated RPC pages
		matchEveryN:     20,                         // 15 matching events * 20 rules = 300 alerts
		ledgerSpan:      25,
	}
	stCatchUp, pagesCatchUp := buildCapacityFixtures(catchUp, 1)
	rpcCatchUp := &fakeRPC{latest: 5025, responses: pagesCatchUp}
	dispCatchUp := &fakeDispatcher{}
	pCatchUp := newTestPoller(rpcCatchUp, stCatchUp, dispCatchUp)

	require.NoError(t, pCatchUp.Poll(ctx))
	assert.Equal(t, 300, pCatchUp.scanned)
	assert.Equal(t, 300*5*4, pCatchUp.evaluations)
	assert.Equal(t, 300, pCatchUp.matched)
	assert.Len(t, stCatchUp.alerts, 300)
	assert.Equal(t, uint32(5025), stCatchUp.state.LastLedger)
}

// BenchmarkPollerCapacityStages isolates the four stages of the evaluation
// pipeline on a fixed workload (100 events, 10 monitors, 5 rules/monitor =
// 5,000 rule evaluations) to identify where time and allocations go.
func BenchmarkPollerCapacityStages(b *testing.B) {
	ctx := context.Background()
	sc := capacityScenario{
		monitors:        10,
		rulesPerMonitor: 5,
		eventsPerPoll:   100,
		pageSize:        100,
		matchEveryN:     0,
		ledgerSpan:      1,
	}

	b.Run("stage1_rpc_fetch_and_decode", func(b *testing.B) {
		_, pages := buildCapacityFixtures(sc, 0)
		templateEvents := pages[0].Events
		src := NewRPCSource(&fakeRPC{latest: 5001}, stellar.DefaultDecoder{})
		watch := []Watch{{ContractID: contractA}}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			src.rpc = &fakeRPC{
				latest: 5001,
				responses: []*stellar.GetEventsResult{{
					Events:       templateEvents,
					LatestLedger: 5001,
				}},
			}
			page, err := src.FetchEvents(ctx, 5000, watch, "", sc.eventsPerPoll)
			if err != nil {
				b.Fatal(err)
			}
			if len(page.Events) != sc.eventsPerPoll {
				b.Fatalf("got %d events, want %d", len(page.Events), sc.eventsPerPoll)
			}
		}
		b.ReportMetric(float64(sc.eventsPerPoll*b.N)/b.Elapsed().Seconds(), "events/s")
	})

	b.Run("stage2_pure_rule_evaluation", func(b *testing.B) {
		st, pages := buildCapacityFixtures(sc, 0)
		src := NewRPCSource(&fakeRPC{latest: 5001, responses: pages}, stellar.DefaultDecoder{})
		page, err := src.FetchEvents(ctx, 5000, []Watch{{ContractID: contractA}}, "", sc.eventsPerPoll)
		if err != nil {
			b.Fatal(err)
		}
		reg := rules.NewRegistry()
		allRules := make([]store.Rule, 0, sc.monitors*sc.rulesPerMonitor)
		for _, m := range st.monitors {
			allRules = append(allRules, st.rules[m.ID]...)
		}

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, ev := range page.Events {
				for _, r := range allRules {
					rCtx := rules.WithRuleID(ctx, r.ID)
					if _, err := reg.Evaluate(rCtx, r.Type, ev, r.Params); err != nil {
						b.Fatal(err)
					}
				}
			}
		}
		totalEvals := float64(len(page.Events) * len(allRules) * b.N)
		b.ReportMetric(totalEvals/b.Elapsed().Seconds(), "evals/s")
	})

	for _, matchCase := range []struct {
		label       string
		matchEveryN int
	}{
		{label: "stage3_ingestor_0pct_match", matchEveryN: 0},
		{label: "stage4_ingestor_10pct_match_alert_write", matchEveryN: 10},
		{label: "stage4_ingestor_100pct_match_alert_write", matchEveryN: 1},
	} {
		matchCase := matchCase
		b.Run(matchCase.label, func(b *testing.B) {
			runSc := sc
			runSc.matchEveryN = matchCase.matchEveryN
			st, pages := buildCapacityFixtures(runSc, 0)
			src := NewRPCSource(&fakeRPC{latest: 5001, responses: pages}, stellar.DefaultDecoder{})
			page, err := src.FetchEvents(ctx, 5000, []Watch{{ContractID: contractA}}, "", runSc.eventsPerPoll)
			if err != nil {
				b.Fatal(err)
			}
			reg := rules.NewRegistry()
			log := slog.New(slog.DiscardHandler)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Clear dedup map each iteration so alert creation is measured honestly.
				st.dedup = map[string]bool{}
				st.alerts = st.alerts[:0]
				disp := &fakeDispatcher{}
				ing := NewIngestor(st, reg, disp, log)
				for _, ev := range page.Events {
					ing.Handle(ctx, ev, st.monitors, HandleOptions{Deliver: true})
				}
			}
			totalEvents := float64(len(page.Events) * b.N)
			totalEvals := totalEvents * float64(sc.monitors*sc.rulesPerMonitor)
			b.ReportMetric(totalEvents/b.Elapsed().Seconds(), "events/s")
			b.ReportMetric(totalEvals/b.Elapsed().Seconds(), "evals/s")
		})
	}
}

// BenchmarkPollerScalingDimensions measures end-to-end Poll throughput across
// monitor count, rules per monitor, and events per poll cycle.
func BenchmarkPollerScalingDimensions(b *testing.B) {
	ctx := context.Background()
	scenarios := []capacityScenario{
		{name: "m1_r5_e50", monitors: 1, rulesPerMonitor: 5, eventsPerPoll: 50, pageSize: 50, matchEveryN: 25, ledgerSpan: 1},
		{name: "m10_r5_e50", monitors: 10, rulesPerMonitor: 5, eventsPerPoll: 50, pageSize: 50, matchEveryN: 25, ledgerSpan: 1},
		{name: "m50_r5_e50", monitors: 50, rulesPerMonitor: 5, eventsPerPoll: 50, pageSize: 50, matchEveryN: 25, ledgerSpan: 1},
		{name: "m10_r20_e50", monitors: 10, rulesPerMonitor: 20, eventsPerPoll: 50, pageSize: 50, matchEveryN: 25, ledgerSpan: 1},
		{name: "m10_r5_e200", monitors: 10, rulesPerMonitor: 5, eventsPerPoll: 200, pageSize: 200, matchEveryN: 25, ledgerSpan: 1},
	}

	for _, sc := range scenarios {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			st, pages := buildCapacityFixtures(sc, 0)
			templatePage := pages[0]
			rpc := &fakeRPC{latest: 5001}
			disp := &fakeDispatcher{}
			p := newTestPoller(rpc, st, disp)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				st.state.LastLedger = 5000
				st.cursors = map[string]store.ContractCursor{}
				st.dedup = map[string]bool{}
				st.alerts = st.alerts[:0]
				disp.dispatched = disp.dispatched[:0]
				rpc.requests = rpc.requests[:0]
				rpc.responses = []*stellar.GetEventsResult{templatePage}

				if err := p.Poll(ctx); err != nil {
					b.Fatal(err)
				}
			}
			totalEvents := float64(sc.eventsPerPoll * b.N)
			totalEvals := totalEvents * float64(sc.monitors*sc.rulesPerMonitor)
			b.ReportMetric(totalEvents/b.Elapsed().Seconds(), "events/s")
			b.ReportMetric(totalEvals/b.Elapsed().Seconds(), "evals/s")
		})
	}
}

// BenchmarkPollerSteadyStateVsCatchUp compares steady-state polling (1 ledger
// per 5s cycle, 20 events in 1 RPC page) against catch-up mode (50 ledgers
// behind, 500 events across 5 paginated getEvents pages).
func BenchmarkPollerSteadyStateVsCatchUp(b *testing.B) {
	ctx := context.Background()
	modes := []capacityScenario{
		{
			name:            "steady_state_1ledger_20events_1page",
			monitors:        10,
			rulesPerMonitor: 5,
			eventsPerPoll:   20,
			pageSize:        100,
			matchEveryN:     20,
			ledgerSpan:      1,
		},
		{
			name:            "catch_up_50ledgers_500events_5pages",
			monitors:        10,
			rulesPerMonitor: 5,
			eventsPerPoll:   500,
			pageSize:        100,
			matchEveryN:     20,
			ledgerSpan:      50,
		},
	}

	for _, sc := range modes {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			st, templatePages := buildCapacityFixtures(sc, 0)
			latest := uint32(5000) + sc.ledgerSpan
			rpc := &fakeRPC{latest: latest}
			disp := &fakeDispatcher{}
			p := newTestPoller(rpc, st, disp)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				st.state.LastLedger = 5000
				st.cursors = map[string]store.ContractCursor{}
				st.dedup = map[string]bool{}
				st.alerts = st.alerts[:0]
				disp.dispatched = disp.dispatched[:0]
				rpc.requests = rpc.requests[:0]
				pagesCopy := make([]*stellar.GetEventsResult, len(templatePages))
				copy(pagesCopy, templatePages)
				rpc.responses = pagesCopy

				if err := p.Poll(ctx); err != nil {
					b.Fatal(err)
				}
			}
			totalEvents := float64(sc.eventsPerPoll * b.N)
			totalEvals := totalEvents * float64(sc.monitors*sc.rulesPerMonitor)
			b.ReportMetric(totalEvents/b.Elapsed().Seconds(), "events/s")
			b.ReportMetric(totalEvals/b.Elapsed().Seconds(), "evals/s")
		})
	}
}
