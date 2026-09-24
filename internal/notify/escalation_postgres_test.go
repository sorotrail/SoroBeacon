package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// escalationTestSchema isolates these tests from the store suite, which
// TRUNCATEs the public tables. `go test ./...` runs both package test binaries
// in parallel against one Postgres, so a shared schema would let one suite
// delete the other's fixtures mid-test.
const escalationTestSchema = "escalation_notify_test"

// noopNotifier succeeds without touching the network: the tests assert on the
// recorded delivery_attempts, not on a real destination.
type noopNotifier struct{}

func (noopNotifier) Send(context.Context, Alert) error { return nil }

// newEscalationTestStore connects to TEST_DATABASE_URL in its own fresh schema
// and migrates it. Skips when the variable is unset, so `go test ./...` works
// without a database while `make test-db` exercises the real store paths.
func newEscalationTestStore(t *testing.T) (*store.Postgres, context.Context) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping escalation integration tests")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()
	_, err = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+escalationTestSchema+" CASCADE")
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+escalationTestSchema)
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	q := u.Query()
	q.Set("options", "-csearch_path="+escalationTestSchema)
	u.RawQuery = q.Encode()
	isolated := u.String()

	require.NoError(t, store.Migrate(isolated))
	st, err := store.NewPostgres(ctx, isolated, store.PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)
	return st, ctx
}

func escalationFactory() *Factory {
	f := &Factory{constructors: map[string]Constructor{}}
	f.Register("mock", func(json.RawMessage) (Notifier, error) { return noopNotifier{}, nil })
	return f
}

func dispatcherFor(t *testing.T, st *store.Postgres) *Dispatcher {
	t.Helper()
	d := NewDispatcher(st, escalationFactory(), slog.New(slog.DiscardHandler))
	d.BaseBackoff = time.Millisecond
	return d
}

// seedEscalation creates a monitor, two mock channels and one alert.
func seedEscalation(t *testing.T, st *store.Postgres, ctx context.Context) (monitorID, chA, chB, alertID int64) {
	t.Helper()
	m := &store.Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	a := &store.Channel{Name: "a", Type: "mock", Config: json.RawMessage(`{}`), Enabled: true}
	b := &store.Channel{Name: "b", Type: "mock", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, a))
	require.NoError(t, st.CreateChannel(ctx, b))
	r := &store.Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	al := &store.Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-1"}
	_, err := st.CreateAlert(ctx, al)
	require.NoError(t, err)
	return m.ID, a.ID, b.ID, al.ID
}

func attemptChannelIDs(t *testing.T, st *store.Postgres, ctx context.Context, alertID int64) []int64 {
	t.Helper()
	attempts, err := st.ListDeliveryAttempts(ctx, alertID, "")
	require.NoError(t, err)
	out := make([]int64, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, a.ChannelID)
	}
	return out
}

// TestDispatchFansOutFlatWhenNoPolicy guards the backwards-compatible half: a
// monitor without a policy keeps notifying every attached channel at once.
func TestDispatchFansOutFlatWhenNoPolicy(t *testing.T) {
	st, ctx := newEscalationTestStore(t)
	monitorID, chA, chB, alertID := seedEscalation(t, st, ctx)
	require.NoError(t, st.SetMonitorChannels(ctx, monitorID, []int64{chA, chB}))

	dispatcherFor(t, st).Dispatch(ctx, Alert{ID: alertID, MonitorID: monitorID})

	assert.ElementsMatch(t, []int64{chA, chB}, attemptChannelIDs(t, st, ctx, alertID))
}

// TestEscalationStepsFireInOrder is the ordering contract: the first step
// fires with the alert, later steps only once their delay has elapsed, and
// always in order.
func TestEscalationStepsFireInOrder(t *testing.T) {
	st, ctx := newEscalationTestStore(t)
	monitorID, chA, chB, alertID := seedEscalation(t, st, ctx)
	_, err := st.SetEscalationPolicy(ctx, monitorID, []store.EscalationStep{
		{DelaySeconds: 0, ChannelIDs: []int64{chA}},
		{DelaySeconds: 60, ChannelIDs: []int64{chB}},
	})
	require.NoError(t, err)

	d := dispatcherFor(t, st)
	d.Dispatch(ctx, Alert{ID: alertID, MonitorID: monitorID})
	assert.Equal(t, []int64{chA}, attemptChannelIDs(t, st, ctx, alertID), "step 0 fires with the alert")

	// The second step is not due yet.
	n, err := d.ProcessDueEscalations(ctx, time.Now())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, []int64{chA}, attemptChannelIDs(t, st, ctx, alertID))

	// Once due, it fires, in order.
	n, err = d.ProcessDueEscalations(ctx, time.Now().Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []int64{chA, chB}, attemptChannelIDs(t, st, ctx, alertID))
}

// TestEscalationResumesAfterRestart pins that scheduling is persisted, not
// held in a time.Timer: a fresh Dispatcher (the restarted process) continues
// the escalation from its stored next-due time.
func TestEscalationResumesAfterRestart(t *testing.T) {
	st, ctx := newEscalationTestStore(t)
	monitorID, chA, chB, alertID := seedEscalation(t, st, ctx)
	_, err := st.SetEscalationPolicy(ctx, monitorID, []store.EscalationStep{
		{DelaySeconds: 0, ChannelIDs: []int64{chA}},
		{DelaySeconds: 60, ChannelIDs: []int64{chB}},
	})
	require.NoError(t, err)

	// The first process starts the escalation and then "dies" before the
	// second step is due.
	dispatcherFor(t, st).Dispatch(ctx, Alert{ID: alertID, MonitorID: monitorID})
	require.Equal(t, []int64{chA}, attemptChannelIDs(t, st, ctx, alertID))

	// A fresh dispatcher picks the persisted schedule back up.
	n, err := dispatcherFor(t, st).ProcessDueEscalations(ctx, time.Now().Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []int64{chA, chB}, attemptChannelIDs(t, st, ctx, alertID))
}

// TestEscalationStopsOnAcknowledge is the stop-on-acknowledge contract: once
// the alert is acknowledged, no later step fires.
func TestEscalationStopsOnAcknowledge(t *testing.T) {
	st, ctx := newEscalationTestStore(t)
	monitorID, chA, chB, alertID := seedEscalation(t, st, ctx)
	_, err := st.SetEscalationPolicy(ctx, monitorID, []store.EscalationStep{
		{DelaySeconds: 0, ChannelIDs: []int64{chA}},
		{DelaySeconds: 60, ChannelIDs: []int64{chB}},
	})
	require.NoError(t, err)

	d := dispatcherFor(t, st)
	d.Dispatch(ctx, Alert{ID: alertID, MonitorID: monitorID})
	require.Equal(t, []int64{chA}, attemptChannelIDs(t, st, ctx, alertID))

	require.NoError(t, st.AcknowledgeAlert(ctx, alertID))

	n, err := d.ProcessDueEscalations(ctx, time.Now().Add(2*time.Minute))
	require.NoError(t, err)
	assert.Zero(t, n, "an acknowledged alert must not escalate further")
	assert.Equal(t, []int64{chA}, attemptChannelIDs(t, st, ctx, alertID))
}
