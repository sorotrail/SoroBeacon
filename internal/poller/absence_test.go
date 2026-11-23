package poller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// t0 is a fixed wall clock the absence tests step by hand. The sweep compares
// times, so a test that slept through a real window would be both slow and
// flaky.
var t0 = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

func seedAbsenceMonitor(st *fakeStore, params string) store.Monitor {
	st.monitors = []store.Monitor{{ID: 1, Name: "m1", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{{ID: 1, MonitorID: 1, Type: rules.TypeAbsenceOfEvent,
		Params: json.RawMessage(params), Enabled: true}}
	return st.monitors[0]
}

func heartbeatEvent(id string, topics ...any) *stellar.DecodedEvent {
	if len(topics) == 0 {
		topics = []any{"heartbeat"}
	}
	return &stellar.DecodedEvent{ID: id, ContractID: contractA, Topics: topics}
}

func TestSweepArmsRuleWithoutFiring(t *testing.T) {
	st := newFakeStore()
	seedAbsenceMonitor(st, `{"event_name": "heartbeat", "window": "30m"}`)
	d := &fakeDispatcher{}
	p := newTestPoller(&fakeRPC{}, st, d)

	require.NoError(t, p.SweepAbsence(context.Background(), t0))

	assert.Empty(t, st.alerts, "a rule that has never been armed must not fire")
	assert.Empty(t, d.dispatched)
	assert.Equal(t, t0, st.absence["1/heartbeat"], "the first sweep arms the clock instead of alerting")
}

func TestSweepFiresAfterWindow(t *testing.T) {
	st := newFakeStore()
	seedAbsenceMonitor(st, `{"event_name": "heartbeat", "window": "30m"}`)
	d := &fakeDispatcher{}
	p := newTestPoller(&fakeRPC{}, st, d)

	require.NoError(t, p.SweepAbsence(context.Background(), t0))
	silent := 31 * time.Minute
	require.NoError(t, p.SweepAbsence(context.Background(), t0.Add(silent)))

	require.Len(t, st.alerts, 1)
	assert.Equal(t, AbsenceEventID(t0), st.alerts[0].EventID, "the window id comes from the instant silence began")
	assert.True(t, st.alerts[0].LedgerClosedAt.IsZero(), "an absence alert has no ledger close time")
	assert.Nil(t, st.monitors[0].LastMatchedAt, "an absence must not stamp last_matched_at as if an event matched")

	require.Len(t, d.dispatched, 1)
	a := d.dispatched[0]
	assert.Equal(t, rules.TypeAbsenceOfEvent, a.RuleType)
	assert.Equal(t, "heartbeat", a.EventName)
	assert.Equal(t, silent, a.Silence)

	var payload struct {
		ContractIDs   []string `json:"contract_ids"`
		EventName     string   `json:"event_name"`
		WindowSeconds int64    `json:"window_seconds"`
		LastSeenAt    string   `json:"last_seen_at"`
		SilentForSecs int64    `json:"silent_for_seconds"`
	}
	require.NoError(t, json.Unmarshal(a.Payload, &payload))
	assert.Equal(t, []string{contractA}, payload.ContractIDs)
	assert.Equal(t, "heartbeat", payload.EventName)
	assert.Equal(t, int64(1800), payload.WindowSeconds)
	assert.Equal(t, int64(1860), payload.SilentForSecs)
	assert.Equal(t, t0.Format(time.RFC3339), payload.LastSeenAt)
}

func TestSweepDoesNotRefireWithinTheSameWindow(t *testing.T) {
	st := newFakeStore()
	seedAbsenceMonitor(st, `{"event_name": "heartbeat", "window": "30m"}`)
	d := &fakeDispatcher{}
	p := newTestPoller(&fakeRPC{}, st, d)

	require.NoError(t, p.SweepAbsence(context.Background(), t0))
	// Three sweeps inside one silent window: the first fires, and the rest
	// collapse onto the same window id, which the (rule_id, event_id) guard
	// turns into a no-op.
	for _, at := range []time.Duration{31 * time.Minute, 45 * time.Minute, 2 * time.Hour} {
		require.NoError(t, p.SweepAbsence(context.Background(), t0.Add(at)))
	}

	assert.Len(t, st.alerts, 1, "one silence, one alert")
	assert.Len(t, d.dispatched, 1, "a deduped alert is not re-dispatched")
	assert.Equal(t, AbsenceEventID(t0), st.alerts[0].EventID)
}

func TestSweepRearmsWhenTheEventReturns(t *testing.T) {
	st := newFakeStore()
	m := seedAbsenceMonitor(st, `{"event_name": "heartbeat", "window": "30m"}`)
	d := &fakeDispatcher{}
	p := newTestPoller(&fakeRPC{}, st, d)

	require.NoError(t, p.SweepAbsence(context.Background(), t0))
	require.NoError(t, p.SweepAbsence(context.Background(), t0.Add(31*time.Minute)))
	require.Len(t, st.alerts, 1)

	// The awaited event returns 40 minutes in. It re-arms the clock and, on
	// its own, can never fire the rule.
	returned := t0.Add(40 * time.Minute)
	p.now = func() time.Time { return returned }
	byContract := map[string][]store.Monitor{contractA: {m}}
	p.handleEvent(context.Background(), heartbeatEvent("ev-hb"), byContract)

	assert.Equal(t, returned, st.absence["1/heartbeat"], "a matching event advances the clock")
	assert.Len(t, st.alerts, 1, "re-arming must not itself alert")

	// 20 minutes of silence is inside the new window...
	require.NoError(t, p.SweepAbsence(context.Background(), t0.Add(60*time.Minute)))
	assert.Len(t, st.alerts, 1)

	// ...31 is not: a new window, and so a new alert with its own id.
	require.NoError(t, p.SweepAbsence(context.Background(), t0.Add(71*time.Minute)))
	require.Len(t, st.alerts, 2)
	assert.Equal(t, AbsenceEventID(returned), st.alerts[1].EventID, "the new window gets its own id")
}

func TestObserveAbsenceIgnoresOtherEvents(t *testing.T) {
	st := newFakeStore()
	m := seedAbsenceMonitor(st, `{"event_name": "heartbeat", "window": "30m"}`)
	p := newTestPoller(&fakeRPC{}, st, &fakeDispatcher{})
	p.now = func() time.Time { return t0.Add(time.Hour) }

	require.NoError(t, p.SweepAbsence(context.Background(), t0))

	byContract := map[string][]store.Monitor{contractA: {m}}
	p.handleEvent(context.Background(), heartbeatEvent("ev-mint", "mint"), byContract)

	assert.Equal(t, t0, st.absence["1/heartbeat"], "only the awaited event re-arms the rule")
}

func TestSweepClockSurvivesRestart(t *testing.T) {
	st := newFakeStore()
	seedAbsenceMonitor(st, `{"event_name": "heartbeat", "window": "30m"}`)

	// One process arms the rule and is thrown away...
	first := newTestPoller(&fakeRPC{}, st, &fakeDispatcher{})
	require.NoError(t, first.SweepAbsence(context.Background(), t0))

	// ...and a fresh one, sharing only the store, resumes measuring from the
	// persisted clock instead of arming a new one.
	d := &fakeDispatcher{}
	restarted := newTestPoller(&fakeRPC{}, st, d)
	require.NoError(t, restarted.SweepAbsence(context.Background(), t0.Add(31*time.Minute)))

	require.Len(t, st.alerts, 1, "the clock, not the process, decides when silence has elapsed")
	assert.Equal(t, AbsenceEventID(t0), st.alerts[0].EventID, "the window id is stable across a restart")
}

func TestSweepSkipsRulesItCannotMeasure(t *testing.T) {
	st := newFakeStore()
	seedAbsenceMonitor(st, `{"event_name": "heartbeat", "window": "soon"}`)
	// An event-driven rule is not the sweep's business, even when it shares a
	// monitor with an absence rule.
	st.rules[1] = append(st.rules[1], store.Rule{ID: 2, MonitorID: 1,
		Type: rules.TypeEventEmitted, Params: json.RawMessage(`{"event_name": "transfer"}`), Enabled: true})

	d := &fakeDispatcher{}
	p := newTestPoller(&fakeRPC{}, st, d)

	require.NoError(t, p.SweepAbsence(context.Background(), t0))
	require.NoError(t, p.SweepAbsence(context.Background(), t0.Add(24*time.Hour)))

	assert.Empty(t, st.alerts, "an unreadable window is skipped, not guessed at")
	assert.Empty(t, d.dispatched)
	assert.Empty(t, st.absence)
}

func TestSweepWithoutMonitorsIsNoop(t *testing.T) {
	st := newFakeStore()
	p := newTestPoller(&fakeRPC{}, st, &fakeDispatcher{})

	assert.NoError(t, p.SweepAbsence(context.Background(), t0))
	assert.Empty(t, st.alerts)
}

func TestAbsenceEventIDIdentifiesTheWindow(t *testing.T) {
	assert.Equal(t, AbsenceEventID(t0), AbsenceEventID(t0),
		"one window, one id: this is what dedups repeated sweeps")
	assert.NotEqual(t, AbsenceEventID(t0), AbsenceEventID(t0.Add(time.Minute)),
		"a re-armed rule must produce a new id, or the alert could never fire again")
	assert.Equal(t, AbsenceEventID(t0), AbsenceEventID(t0.In(time.FixedZone("elsewhere", 3600))),
		"the same instant in another zone must dedup to the same alert")
}
