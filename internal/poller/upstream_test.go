package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

func (f *fakeStore) ListAlerts(_ context.Context, filter store.AlertFilter) ([]store.Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Alert
	for _, a := range f.alerts {
		if a.RetractedAt != nil {
			continue
		}
		if filter.Network != "" && a.Network != filter.Network {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

type fakeUpstreamSource struct {
	latest              uint32
	chainHead           uint32
	upstreamLag         uint32
	consecutiveFailures int
	retractedFrom       uint32
	retractedIDs        []string
	pages               map[string]FetchPage
	cursorErrors        map[string]error
	fetchCalls          []string
}

func (u *fakeUpstreamSource) LatestLedger(_ context.Context) (uint32, error) {
	return u.latest, nil
}

func (u *fakeUpstreamSource) FetchEvents(_ context.Context, startLedger uint32, _ []Watch, cursor string, _ int) (FetchPage, error) {
	callKey := fmt.Sprintf("start=%d,cursor=%s", startLedger, cursor)
	u.fetchCalls = append(u.fetchCalls, callKey)
	if err, ok := u.cursorErrors[cursor]; ok && cursor != "" {
		return FetchPage{}, err
	}
	if page, ok := u.pages[cursor]; ok {
		return page, nil
	}
	return FetchPage{LatestLedger: u.latest}, nil
}

func (u *fakeUpstreamSource) UpstreamStatus() (chainHead uint32, upstreamLag uint32, consecutiveFailures int) {
	return u.chainHead, u.upstreamLag, u.consecutiveFailures
}

func (u *fakeUpstreamSource) ConsumeRetractions() (retractedFromLedger uint32, retractedEventIDs []string) {
	from := u.retractedFrom
	ids := u.retractedIDs
	u.retractedFrom = 0
	u.retractedIDs = nil
	return from, ids
}

func TestUpstreamLagReflectedInPositionAndMetrics(t *testing.T) {
	st := newFakeStore()
	st.monitors = []store.Monitor{{ID: 1, Name: "DEX", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{{
		ID: 10, MonitorID: 1, Type: rules.TypeEventEmitted,
		Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true,
	}}
	st.state = store.IngestState{LastLedger: 999}

	src := &fakeUpstreamSource{
		latest:      1000,
		chainHead:   1500,
		upstreamLag: 500,
		pages: map[string]FetchPage{
			"": {LatestLedger: 1000},
		},
	}
	disp := &fakeDispatcher{}
	m := metrics.New()
	p := New(src, st, rules.NewRegistry(), disp, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil))).WithMetrics(m)

	require.NoError(t, p.Poll(context.Background()))

	pos := p.Position()
	require.True(t, pos.Ready())
	assert.Equal(t, uint32(1000), pos.LastProcessedLedger)
	assert.Equal(t, uint32(1500), pos.LatestChainLedger, "LatestChainLedger must reflect the chain head so readiness sees upstream lag")
	assert.Equal(t, int64(500), pos.Lag(), "Position.Lag must include upstream SoroTrail lag")
}

func TestUpstreamRetractionMarksAlertRetractedAndNotifiesConsumer(t *testing.T) {
	st := newFakeStore()
	st.monitors = []store.Monitor{{ID: 1, Name: "Vault", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{{
		ID: 10, MonitorID: 1, Type: rules.TypeEventEmitted,
		Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true,
	}}
	st.state = store.IngestState{LastLedger: 149}

	src := &fakeUpstreamSource{
		latest:    150,
		chainHead: 150,
		pages: map[string]FetchPage{
			"": {
				LatestLedger: 150,
				Events: []*stellar.DecodedEvent{
					{
						ID:         "ev-reorged",
						ContractID: contractA,
						Ledger:     150,
						TxHash:     "tx-orphaned",
						Topics:     []any{map[string]any{"symbol": "transfer"}},
						Value:      map[string]any{"amount": "100"},
					},
				},
			},
		},
	}
	disp := &fakeDispatcher{}
	p := New(src, st, rules.NewRegistry(), disp, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Cycle 1: alert is created and delivered.
	require.NoError(t, p.Poll(context.Background()))
	require.Len(t, st.alerts, 1)
	require.Nil(t, st.alerts[0].RetractedAt)
	require.Len(t, disp.dispatched, 1)
	assert.Equal(t, "ev-reorged", disp.dispatched[0].EventID)

	// Cycle 2: upstream SoroTrail retracts ledger 150 / ev-reorged.
	src.retractedFrom = 150
	src.retractedIDs = []string{"ev-reorged"}
	src.pages[""] = FetchPage{LatestLedger: 151}
	require.NoError(t, p.Poll(context.Background()))

	require.NotNil(t, st.alerts[0].RetractedAt, "retracted alert must be marked in the store")
	require.Len(t, disp.dispatched, 2, "a delivered alert that is later retracted must emit a retraction notice")
	retractNotice := disp.dispatched[1]
	assert.Equal(t, "ev-reorged:retracted", retractNotice.EventID)
	assert.Equal(t, "retraction", retractNotice.RuleType)
	assert.Contains(t, retractNotice.EventName, "[RETRACTED]")
}

func TestUpstreamInvalidCursorReanchorsFromCheckpoint(t *testing.T) {
	st := newFakeStore()
	st.monitors = []store.Monitor{{ID: 1, Name: "Token", ContractIDs: []string{contractA}, Enabled: true}}
	st.rules[1] = []store.Rule{{
		ID: 10, MonitorID: 1, Type: rules.TypeEventEmitted,
		Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true,
	}}
	st.state = store.IngestState{LastLedger: 100, LastCursor: "corrupt-cursor"}

	src := &fakeUpstreamSource{
		latest:    120,
		chainHead: 120,
		cursorErrors: map[string]error{
			"corrupt-cursor": fmt.Errorf("%w: HTTP 400 invalid cursor", ErrInvalidCursor),
		},
		pages: map[string]FetchPage{
			"": {
				LatestLedger: 120,
				Events: []*stellar.DecodedEvent{
					{
						ID:         "ev-recovered",
						ContractID: contractA,
						Ledger:     115,
						Topics:     []any{map[string]any{"symbol": "transfer"}},
					},
				},
			},
		},
	}
	disp := &fakeDispatcher{}
	p := New(src, st, rules.NewRegistry(), disp, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.NoError(t, p.Poll(context.Background()))
	require.Len(t, src.fetchCalls, 2, "poller must retry without cursor after ErrInvalidCursor")
	assert.Equal(t, "start=101,cursor=corrupt-cursor", src.fetchCalls[0])
	assert.Equal(t, "start=101,cursor=", src.fetchCalls[1])
	require.Len(t, st.alerts, 1)
	assert.Equal(t, "ev-recovered", st.alerts[0].EventID)
	assert.Equal(t, uint32(120), st.state.LastLedger)
	assert.Empty(t, st.state.LastCursor)
}
