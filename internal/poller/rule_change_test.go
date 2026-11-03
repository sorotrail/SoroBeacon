package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type ruleMutatingStore struct {
	*fakeStore
	onFirstAlert func()
	called       bool
}

func (s *ruleMutatingStore) CreateAlert(ctx context.Context, a *store.Alert) (store.AlertOutcome, error) {
	out, err := s.fakeStore.CreateAlert(ctx, a)
	if !s.called && s.onFirstAlert != nil {
		s.called = true
		s.onFirstAlert()
	}
	return out, err
}

func TestPollerRuleSnapshotConsistentWithinCycleAndRefreshedNextCycle(t *testing.T) {
	base := newFakeStore()
	base.state.LastLedger = 100
	base.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	base.rules[1] = []store.Rule{{
		ID:        10,
		MonitorID: 1,
		Type:      rules.TypeEventEmitted,
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
		Version:   1,
	}}

	// Mutate the rule in the backing store right after the first event in the
	// batch fires an alert. Because handleEvent snapshots rules per poll cycle,
	// the second event in the same batch must still evaluate against version 1.
	st := &ruleMutatingStore{
		fakeStore: base,
		onFirstAlert: func() {
			base.mu.Lock()
			defer base.mu.Unlock()
			base.rules[1] = []store.Rule{{
				ID:        10,
				MonitorID: 1,
				Type:      rules.TypeEventEmitted,
				Params:    json.RawMessage(`{"event_name":"mint"}`),
				Enabled:   true,
				Version:   2,
			}}
		},
	}

	rpc := &fakeRPC{
		latest: 110,
		responses: []*stellar.GetEventsResult{
			{
				Events: []stellar.Event{
					transferEvent("ev-1", 101, "10"),
					transferEvent("ev-2", 102, "20"),
				},
				LatestLedger: 105,
			},
			{
				Events: []stellar.Event{
					transferEvent("ev-3", 106, "30"),
					{
						ID: "ev-4", ContractID: contractA, Ledger: 107, Type: "contract",
						TopicJSON: []json.RawMessage{json.RawMessage(`{"symbol":"mint"}`)},
						ValueJSON: json.RawMessage(`{"i128":"40"}`),
					},
				},
				LatestLedger: 110,
			},
		},
	}

	p := New(NewRPCSource(rpc, stellar.DefaultDecoder{}), st, rules.NewRegistry(), &fakeDispatcher{}, time.Second, slog.New(slog.DiscardHandler))

	// Cycle 1: both transfer events match v1 ("transfer") even though the rule
	// changed to v2 ("mint") in the store after ev-1.
	require.NoError(t, p.Poll(context.Background()))
	require.Len(t, base.alerts, 2, "both events in cycle 1 must evaluate against the cycle's v1 rule snapshot")
	assert.Equal(t, 1, base.alerts[0].RuleVersion)
	assert.Equal(t, 1, base.alerts[1].RuleVersion)

	// Cycle 2: loads the fresh v2 rule ("mint"), so ev-3 ("transfer") is ignored
	// and ev-4 ("mint") fires with RuleVersion == 2.
	require.NoError(t, p.Poll(context.Background()))
	require.Len(t, base.alerts, 3)
	assert.Equal(t, "ev-4", base.alerts[2].EventID)
	assert.Equal(t, 2, base.alerts[2].RuleVersion)
}

func TestPollerRuleChangeResetsFrequencyAndAbsenceAndPreservesHistory(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + t.TempDir() + "/rules.db"
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(ctx, url)
	require.NoError(t, err)
	defer st.Close()

	m := &store.Monitor{Name: "m-live", ContractIDs: []string{contractA}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	require.NoError(t, st.SetIngestState(ctx, "", store.IngestState{LastLedger: 100}))

	freqRule := &store.Rule{
		MonitorID: m.ID,
		Type:      rules.TypeFrequencyThreshold,
		Params:    json.RawMessage(`{"event_name":"transfer","count":2,"window":"10m"}`),
		Enabled:   true,
	}
	require.NoError(t, st.CreateRule(ctx, freqRule))
	require.Equal(t, 1, freqRule.Version)

	absRule := &store.Rule{
		MonitorID: m.ID,
		Type:      rules.TypeAbsenceOfEvent,
		Params:    json.RawMessage(`{"event_name":"heartbeat","window":"5m"}`),
		Enabled:   true,
	}
	require.NoError(t, st.CreateRule(ctx, absRule))
	require.Equal(t, 1, absRule.Version)

	now := time.Unix(1_700_000_000, 0).UTC()
	reg := rules.NewRegistry()
	freqEval := rules.NewFrequencyThreshold().WithClock(func() time.Time { return now })
	reg.Register(rules.TypeFrequencyThreshold, freqEval)

	rpc := &fakeRPC{
		latest: 120,
		responses: []*stellar.GetEventsResult{
			{
				Events: []stellar.Event{
					transferEvent("ev-1", 101, "10"),
					transferEvent("ev-2", 102, "20"),
				},
				LatestLedger: 105,
			},
			{
				Events: []stellar.Event{
					transferEvent("ev-3", 106, "30"),
					transferEvent("ev-4", 107, "40"),
				},
				LatestLedger: 110,
			},
		},
	}

	log := slog.New(slog.DiscardHandler)
	p := New(NewRPCSource(rpc, stellar.DefaultDecoder{}), st, reg, &fakeDispatcher{}, time.Second, log)
	p.now = func() time.Time { return now }

	// Poll 1: two transfers cross count=2 under v1 and create one v1 alert.
	require.NoError(t, p.Poll(ctx))
	alerts, err := st.ListAlerts(ctx, store.AlertFilter{RuleID: freqRule.ID, Sort: "created_at_asc"})
	require.NoError(t, err)
	require.Len(t, alerts, 1)
	assert.Equal(t, 1, alerts[0].RuleVersion)

	// Arm the absence rule at t=now, then advance 4m (not yet 5m).
	require.NoError(t, p.SweepAbsence(ctx, now))
	now = now.Add(4 * time.Minute)

	// Edit both rules:
	// 1. freqRule remains count=2, window=5m (condition changed -> version 2).
	//    Without state reset, freqEval would still be armed from v1 inside the
	//    10m window and suppress new crossings!
	freqRule.Params = json.RawMessage(`{"event_name":"transfer","count":2,"window":"5m"}`)
	require.NoError(t, st.UpdateRule(ctx, freqRule))
	assert.Equal(t, 2, freqRule.Version)
	reg.ResetRuleState(freqRule.ID)

	// 2. absRule window changed from 5m to 3m (condition changed -> version 2).
	//    Because UpdateRule clears rule_absence_state, the next SweepAbsence
	//    must re-arm from now instead of firing immediately on the 4m pre-edit silence.
	absRule.Params = json.RawMessage(`{"event_name":"heartbeat","window":"3m"}`)
	require.NoError(t, st.UpdateRule(ctx, absRule))
	assert.Equal(t, 2, absRule.Version)

	require.NoError(t, p.SweepAbsence(ctx, now))
	absAlerts, err := st.ListAlerts(ctx, store.AlertFilter{RuleID: absRule.ID})
	require.NoError(t, err)
	assert.Empty(t, absAlerts, "edited absence rule must re-arm from edit time, not fire on pre-edit silence")

	// Advance 3m01s past the re-arm baseline: now the v2 absence rule fires and stamps RuleVersion=2.
	now = now.Add(3*time.Minute + time.Second)
	require.NoError(t, p.SweepAbsence(ctx, now))
	absAlerts, err = st.ListAlerts(ctx, store.AlertFilter{RuleID: absRule.ID})
	require.NoError(t, err)
	require.Len(t, absAlerts, 1)
	assert.Equal(t, 2, absAlerts[0].RuleVersion)

	// Poll 2: two new transfers arrive under v2 of freqRule and fire a v2 alert,
	// while the historical v1 alert remains intact.
	require.NoError(t, p.Poll(ctx))
	alerts, err = st.ListAlerts(ctx, store.AlertFilter{RuleID: freqRule.ID, Sort: "created_at_asc"})
	require.NoError(t, err)
	require.Len(t, alerts, 2, "historical v1 alert is preserved alongside new v2 alert")
	assert.Equal(t, 1, alerts[0].RuleVersion)
	assert.Equal(t, 2, alerts[1].RuleVersion)
}
