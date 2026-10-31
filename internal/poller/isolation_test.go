package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

type customRuleFunc func(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error)

func (f customRuleFunc) Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	return f(ctx, ev, params)
}

func (customRuleFunc) Validate(json.RawMessage) error { return nil }

func TestPollerIsolatesPanickingAndSlowRulesAcrossWorkspaces(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	st.state.LastLedger = 5500

	// Workspace 1 has both a panicking rule and a slow rule.
	// Workspace 2 watches the same contract with a healthy transfer rule.
	st.monitors = []store.Monitor{
		{ID: 1, Name: "workspace-bad", ContractIDs: []string{contractA}, Enabled: true},
		{ID: 2, Name: "workspace-healthy", ContractIDs: []string{contractA}, Enabled: true},
	}
	st.rules[1] = []store.Rule{
		{ID: 11, MonitorID: 1, Type: "panicking_rule", Params: json.RawMessage(`{}`), Enabled: true},
		{ID: 12, MonitorID: 1, Type: "slow_rule", Params: json.RawMessage(`{}`), Enabled: true},
	}
	st.rules[2] = []store.Rule{
		{ID: 21, MonitorID: 2, Type: rules.TypeEventEmitted, Params: json.RawMessage(`{"event_name":"transfer"}`), Enabled: true},
	}

	reg := rules.NewRegistry().
		WithEvalTimeout(15 * time.Millisecond).
		WithQuarantineThreshold(2)

	var panicCalls atomic.Int32
	reg.Register("panicking_rule", customRuleFunc(func(_ context.Context, _ *stellar.DecodedEvent, _ json.RawMessage) (bool, error) {
		panicCalls.Add(1)
		panic("boom on malformed value")
	}))
	reg.Register("slow_rule", customRuleFunc(func(ctx context.Context, _ *stellar.DecodedEvent, _ json.RawMessage) (bool, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}))

	rpc := &fakeRPC{
		latest: 5505,
		responses: []*stellar.GetEventsResult{{
			LatestLedger: 5505,
			Events: []stellar.Event{
				transferEvent("ev-1", 5501, "100"),
				transferEvent("ev-2", 5502, "200"),
				transferEvent("ev-3", 5503, "300"),
			},
		}},
	}

	m := metrics.New()
	disp := &fakeDispatcher{}
	p := New(NewRPCSource(rpc, stellar.DefaultDecoder{}), st, reg, disp, time.Second, slog.New(slog.DiscardHandler)).
		WithMetrics(m)

	require.NoError(t, p.Poll(ctx))

	// Both pathological rules quarantined after 2 events; the 3rd event skipped them.
	assert.Equal(t, int32(2), panicCalls.Load(), "panicking rule must stop running once quarantined")
	q11, ok := reg.QuarantineState(11)
	require.True(t, ok)
	assert.True(t, q11.Quarantined)
	assert.Equal(t, rules.QuarantineReasonPanic, q11.Reason)

	q12, ok := reg.QuarantineState(12)
	require.True(t, ok)
	assert.True(t, q12.Quarantined)
	assert.Equal(t, rules.QuarantineReasonTimeout, q12.Reason)

	// Workspace 2 received all 3 alerts and the contract cursor advanced cleanly.
	assert.Len(t, st.alerts, 3, "healthy monitor in workspace 2 must fire for all 3 events")
	assert.Len(t, disp.dispatched, 3)
	assert.Equal(t, uint32(5505), st.state.LastLedger, "pathological rules must not hold back the ledger checkpoint")

	// Verify the existing panic metric and per-rule error metrics were incremented.
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	assert.Contains(t, body, `sorobeacon_poll_panics_total{network=""} 2`)
	assert.Contains(t, body, `sorobeacon_rule_errors_total{network="",rule_type="panicking_rule"} 3`)
	assert.Contains(t, body, `sorobeacon_rule_errors_total{network="",rule_type="slow_rule"} 3`)
}
