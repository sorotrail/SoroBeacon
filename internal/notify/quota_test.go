package notify

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

var quotaNow = time.Date(2026, 6, 10, 9, 30, 0, 0, time.UTC)

// budgetFixture is a real SQLite store holding one monitor, so the budget
// behaves exactly as it does in production (durable, atomic) while the rest of
// dispatch stays on the usual in-memory fake.
func budgetFixture(t *testing.T, limit *int) (*store.SQLite, string, int64) {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "quota.db")
	require.NoError(t, store.Migrate(url))
	ctx := context.Background()
	st, err := store.NewSQLite(ctx, url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	m := &store.Monitor{Name: "vault", Enabled: true, Network: "testnet"}
	require.NoError(t, st.CreateMonitor(ctx, m))
	if limit != nil {
		require.NoError(t, st.SetDeliveryBudget(ctx, m.ID, limit))
	}
	return st, url, m.ID
}

func quotaDispatcher(t *testing.T, b store.DeliveryBudgets) (*Dispatcher, *fakeDispatchStore, *mockNotifier) {
	t.Helper()
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{}
	d := newTestDispatcher(t, st, n).
		WithDeliveryBudgets(b).
		WithClock(func() time.Time { return quotaNow })
	return d, st, n
}

func quotaAlert(id, monitorID int64) Alert {
	return Alert{ID: id, MonitorID: monitorID, MonitorName: "vault", Severity: "warning"}
}

func intp(n int) *int { return &n }

func TestDeliveryBudgetStopsDeliveryAndAnnouncesOnce(t *testing.T) {
	budgets, _, monitorID := budgetFixture(t, intp(2))
	d, st, n := quotaDispatcher(t, budgets)
	ctx := context.Background()

	d.Dispatch(ctx, quotaAlert(1, monitorID))
	d.Dispatch(ctx, quotaAlert(2, monitorID))
	require.Equal(t, 2, n.calls, "alerts inside the budget are delivered")
	assert.Empty(t, st.suppressed)

	// The first alert past the budget is replaced by the exhaustion notice.
	d.Dispatch(ctx, quotaAlert(3, monitorID))
	require.Equal(t, 3, n.calls, "the exhaustion notice goes out through the channels")
	assert.Contains(t, n.lastSent.Digest, "Delivery budget exhausted")
	assert.Contains(t, n.lastSent.Digest, "limit 2")
	assert.Equal(t, "critical", n.lastSent.Severity, "the notice must not be hidden by a minimum severity")

	// Everything after that is stored but silent: no repeated notice.
	for id := int64(4); id <= 10; id++ {
		d.Dispatch(ctx, quotaAlert(id, monitorID))
	}
	assert.Equal(t, 3, n.calls, "nothing is delivered after the notice, and the notice is not repeated")
	assert.Len(t, st.suppressed, 8, "every alert past the budget is marked suppressed with the reason")
	assert.Contains(t, st.suppressed[0], "delivery budget exhausted")
}

func TestDeliveryBudgetDefaultsToUnlimited(t *testing.T) {
	budgets, _, monitorID := budgetFixture(t, nil)
	d, st, n := quotaDispatcher(t, budgets)
	for id := int64(1); id <= 25; id++ {
		d.Dispatch(context.Background(), quotaAlert(id, monitorID))
	}
	assert.Equal(t, 25, n.calls, "a monitor with no budget is unaffected")
	assert.Empty(t, st.suppressed)
}

func TestDeliveryBudgetRefillsAtNextUTCDay(t *testing.T) {
	budgets, _, monitorID := budgetFixture(t, intp(1))
	st := &fakeDispatchStore{channels: []store.Channel{mockChannel(1)}}
	n := &mockNotifier{}
	now := quotaNow
	d := newTestDispatcher(t, st, n).WithDeliveryBudgets(budgets).WithClock(func() time.Time { return now })
	ctx := context.Background()

	d.Dispatch(ctx, quotaAlert(1, monitorID))
	d.Dispatch(ctx, quotaAlert(2, monitorID)) // notice
	d.Dispatch(ctx, quotaAlert(3, monitorID)) // silent
	require.Equal(t, 2, n.calls)

	now = now.Add(24 * time.Hour)
	d.Dispatch(ctx, quotaAlert(4, monitorID))
	assert.Equal(t, 3, n.calls, "the budget refills when the UTC day rolls over")
	assert.Empty(t, n.lastSent.Digest, "and it is a normal alert again, not a notice")
}

func TestDeliveryBudgetSurvivesRestart(t *testing.T) {
	budgets, url, monitorID := budgetFixture(t, intp(1))
	d, _, n := quotaDispatcher(t, budgets)
	d.Dispatch(context.Background(), quotaAlert(1, monitorID))
	d.Dispatch(context.Background(), quotaAlert(2, monitorID)) // notice
	require.Equal(t, 2, n.calls)
	budgets.Close()

	reopened, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(reopened.Close)
	d2, st2, n2 := quotaDispatcher(t, reopened)
	d2.Dispatch(context.Background(), quotaAlert(3, monitorID))
	assert.Equal(t, 0, n2.calls, "a restart must not reopen the floodgate, nor repeat the notice")
	assert.Len(t, st2.suppressed, 1)
}

func TestDeliveryBudgetManualResetReopensDelivery(t *testing.T) {
	budgets, _, monitorID := budgetFixture(t, intp(1))
	d, _, n := quotaDispatcher(t, budgets)
	ctx := context.Background()
	d.Dispatch(ctx, quotaAlert(1, monitorID))
	d.Dispatch(ctx, quotaAlert(2, monitorID))
	require.Equal(t, 2, n.calls)

	require.NoError(t, budgets.ResetDeliveryBudget(ctx, monitorID, quotaNow))
	d.Dispatch(ctx, quotaAlert(3, monitorID))
	assert.Equal(t, 3, n.calls)
	assert.Empty(t, n.lastSent.Digest)
}

type failingBudgets struct{ store.DeliveryBudgets }

func (failingBudgets) ConsumeDelivery(context.Context, int64, time.Time) (store.BudgetDecision, error) {
	return store.BudgetDecision{}, errors.New("db down")
}

func TestDeliveryBudgetFailsOpenWhenStoreErrors(t *testing.T) {
	d, _, n := quotaDispatcher(t, failingBudgets{})
	d.Dispatch(context.Background(), quotaAlert(1, 1))
	assert.Equal(t, 1, n.calls, "a broken budget store must not silence alerts")
}

func TestDeliveryBudgetNoticeNeverCarriesChannelConfig(t *testing.T) {
	budgets, _, monitorID := budgetFixture(t, intp(1))
	d, st, n := quotaDispatcher(t, budgets)
	ch := mockChannel(1)
	ch.Config = []byte(`{"url":"https://hooks.example.com/SECRET-TOKEN"}`)
	st.channels = []store.Channel{ch}
	d.Dispatch(context.Background(), quotaAlert(1, monitorID))
	d.Dispatch(context.Background(), quotaAlert(2, monitorID))
	assert.NotContains(t, n.lastSent.Digest, "SECRET-TOKEN")
	for _, reason := range st.suppressed {
		assert.NotContains(t, reason, "SECRET-TOKEN")
	}
}
