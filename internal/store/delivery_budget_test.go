package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intp(n int) *int { return &n }

var budgetDay = time.Date(2026, 6, 10, 9, 30, 0, 0, time.UTC)

func TestDecideDeliveryRule(t *testing.T) {
	t.Run("no limit is unlimited and never changes state", func(t *testing.T) {
		in := DeliveryBudget{Used: 7}
		out, d := decideDelivery(in, budgetDay)
		assert.True(t, d.Allowed)
		assert.False(t, d.Announce)
		assert.Equal(t, in, out)
	})
	t.Run("the limit-th delivery is allowed and the next is denied", func(t *testing.T) {
		b := DeliveryBudget{Limit: intp(2)}
		var d BudgetDecision
		b, d = decideDelivery(b, budgetDay)
		assert.True(t, d.Allowed)
		b, d = decideDelivery(b, budgetDay)
		assert.True(t, d.Allowed)
		assert.Equal(t, 2, d.Used)
		assert.False(t, b.Exhausted(), "spending the last unit is not yet exhaustion")
		b, d = decideDelivery(b, budgetDay)
		assert.False(t, d.Allowed)
		assert.True(t, d.Announce)
		assert.True(t, b.Exhausted())
	})
	t.Run("exhaustion is announced exactly once", func(t *testing.T) {
		b := DeliveryBudget{Limit: intp(1)}
		b, _ = decideDelivery(b, budgetDay)
		announced := 0
		for i := 0; i < 20; i++ {
			var d BudgetDecision
			b, d = decideDelivery(b, budgetDay.Add(time.Duration(i)*time.Minute))
			assert.False(t, d.Allowed)
			if d.Announce {
				announced++
			}
		}
		assert.Equal(t, 1, announced)
	})
	t.Run("a new UTC day refills the budget and re-arms the announcement", func(t *testing.T) {
		b := DeliveryBudget{Limit: intp(1)}
		b, _ = decideDelivery(b, budgetDay)
		b, _ = decideDelivery(b, budgetDay)
		require.True(t, b.Exhausted())

		next := budgetDay.Add(24 * time.Hour)
		b, d := decideDelivery(b, next)
		assert.True(t, d.Allowed)
		assert.False(t, b.Exhausted())
		assert.Equal(t, time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC), b.WindowStart)

		_, d = decideDelivery(b, next)
		assert.True(t, d.Announce, "a second exhaustion on a later day is announced again")
	})
}

func TestDeliveryBudgetRemaining(t *testing.T) {
	assert.Equal(t, -1, DeliveryBudget{}.Remaining())
	assert.Equal(t, 3, DeliveryBudget{Limit: intp(5), Used: 2}.Remaining())
	assert.Equal(t, 0, DeliveryBudget{Limit: intp(5), Used: 9}.Remaining(), "never negative")
}

func TestDeliveryBudgetSQLite(t *testing.T) {
	runDeliveryBudgetConformance(t, newTestSQLite)
}

func TestDeliveryBudgetPostgres(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres delivery budget tests")
	}
	runDeliveryBudgetConformance(t, newTestPostgres)
}

// runDeliveryBudgetConformance runs against both backends, so the SQL of one
// cannot drift from the other.
func runDeliveryBudgetConformance(t *testing.T, newStore conformanceFactory) {
	newMonitor := func(t *testing.T) (conformanceStore, context.Context, int64) {
		t.Helper()
		st := newStore(t)
		ctx := context.Background()
		m := &Monitor{Name: "vault", Enabled: true, Network: "testnet"}
		require.NoError(t, st.CreateMonitor(ctx, m))
		return st, ctx, m.ID
	}

	t.Run("defaults to unlimited", func(t *testing.T) {
		st, ctx, id := newMonitor(t)
		b, err := st.GetDeliveryBudget(ctx, id, budgetDay)
		require.NoError(t, err)
		assert.Nil(t, b.Limit)
		assert.Equal(t, -1, b.Remaining())
		for i := 0; i < 5; i++ {
			d, err := st.ConsumeDelivery(ctx, id, budgetDay)
			require.NoError(t, err)
			assert.True(t, d.Allowed)
		}
	})

	t.Run("stops after the limit and announces once", func(t *testing.T) {
		st, ctx, id := newMonitor(t)
		require.NoError(t, st.SetDeliveryBudget(ctx, id, intp(3)))
		announced := 0
		allowed := 0
		for i := 0; i < 10; i++ {
			d, err := st.ConsumeDelivery(ctx, id, budgetDay)
			require.NoError(t, err)
			if d.Allowed {
				allowed++
			}
			if d.Announce {
				announced++
			}
		}
		assert.Equal(t, 3, allowed)
		assert.Equal(t, 1, announced)
		b, err := st.GetDeliveryBudget(ctx, id, budgetDay)
		require.NoError(t, err)
		assert.True(t, b.Exhausted())
		assert.Equal(t, 0, b.Remaining())
	})

	t.Run("concurrent consumers never overspend", func(t *testing.T) {
		st, ctx, id := newMonitor(t)
		require.NoError(t, st.SetDeliveryBudget(ctx, id, intp(5)))
		var mu sync.Mutex
		allowed, announced := 0, 0
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d, err := st.ConsumeDelivery(ctx, id, budgetDay)
				if !assert.NoError(t, err) {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if d.Allowed {
					allowed++
				}
				if d.Announce {
					announced++
				}
			}()
		}
		wg.Wait()
		assert.Equal(t, 5, allowed)
		assert.Equal(t, 1, announced)
	})

	t.Run("rolls over at the next UTC day", func(t *testing.T) {
		st, ctx, id := newMonitor(t)
		require.NoError(t, st.SetDeliveryBudget(ctx, id, intp(1)))
		_, _ = st.ConsumeDelivery(ctx, id, budgetDay)
		d, err := st.ConsumeDelivery(ctx, id, budgetDay)
		require.NoError(t, err)
		require.False(t, d.Allowed)

		tomorrow := budgetDay.Add(24 * time.Hour)
		b, err := st.GetDeliveryBudget(ctx, id, tomorrow)
		require.NoError(t, err)
		assert.False(t, b.Exhausted(), "a read after rollover shows the refilled budget")
		d, err = st.ConsumeDelivery(ctx, id, tomorrow)
		require.NoError(t, err)
		assert.True(t, d.Allowed)
	})

	t.Run("manual reset un-exhausts", func(t *testing.T) {
		st, ctx, id := newMonitor(t)
		require.NoError(t, st.SetDeliveryBudget(ctx, id, intp(1)))
		_, _ = st.ConsumeDelivery(ctx, id, budgetDay)
		_, _ = st.ConsumeDelivery(ctx, id, budgetDay)
		require.NoError(t, st.ResetDeliveryBudget(ctx, id, budgetDay))
		b, err := st.GetDeliveryBudget(ctx, id, budgetDay)
		require.NoError(t, err)
		assert.False(t, b.Exhausted())
		assert.Equal(t, 0, b.Used)
		d, err := st.ConsumeDelivery(ctx, id, budgetDay)
		require.NoError(t, err)
		assert.True(t, d.Allowed)
	})

	t.Run("raising the limit un-exhausts, lowering keeps it", func(t *testing.T) {
		st, ctx, id := newMonitor(t)
		require.NoError(t, st.SetDeliveryBudget(ctx, id, intp(2)))
		for i := 0; i < 3; i++ {
			_, _ = st.ConsumeDelivery(ctx, id, budgetDay)
		}
		require.NoError(t, st.SetDeliveryBudget(ctx, id, intp(2)))
		b, _ := st.GetDeliveryBudget(ctx, id, budgetDay)
		assert.True(t, b.Exhausted(), "an unchanged limit must not reopen delivery")

		require.NoError(t, st.SetDeliveryBudget(ctx, id, intp(10)))
		b, _ = st.GetDeliveryBudget(ctx, id, budgetDay)
		assert.False(t, b.Exhausted())
		assert.Equal(t, 8, b.Remaining())
	})

	t.Run("clearing the budget makes the monitor unlimited again", func(t *testing.T) {
		st, ctx, id := newMonitor(t)
		require.NoError(t, st.SetDeliveryBudget(ctx, id, intp(1)))
		_, _ = st.ConsumeDelivery(ctx, id, budgetDay)
		require.NoError(t, st.SetDeliveryBudget(ctx, id, nil))
		b, err := st.GetDeliveryBudget(ctx, id, budgetDay)
		require.NoError(t, err)
		assert.Nil(t, b.Limit)
		assert.Equal(t, 0, b.Used)
		d, err := st.ConsumeDelivery(ctx, id, budgetDay)
		require.NoError(t, err)
		assert.True(t, d.Allowed)
	})

	t.Run("rejects non-positive limits and unknown monitors", func(t *testing.T) {
		st, ctx, id := newMonitor(t)
		assert.ErrorIs(t, st.SetDeliveryBudget(ctx, id, intp(0)), ErrInvalidDeliveryBudget)
		assert.ErrorIs(t, st.SetDeliveryBudget(ctx, id, intp(-4)), ErrInvalidDeliveryBudget)
		assert.ErrorIs(t, st.SetDeliveryBudget(ctx, id+999, intp(1)), ErrNotFound)
		assert.ErrorIs(t, st.ResetDeliveryBudget(ctx, id+999, budgetDay), ErrNotFound)
		_, err := st.GetDeliveryBudget(ctx, id+999, budgetDay)
		assert.ErrorIs(t, err, ErrNotFound)
		_, err = st.ConsumeDelivery(ctx, id+999, budgetDay)
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

// A restart must not reopen the floodgate: consumption lives in the database,
// so a fresh store over the same file sees the spent budget.
func TestDeliveryBudgetSurvivesRestartSQLite(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "restart.db")
	require.NoError(t, Migrate(url))
	ctx := context.Background()

	st, err := NewSQLite(ctx, url)
	require.NoError(t, err)
	m := &Monitor{Name: "vault", Enabled: true, Network: "testnet"}
	require.NoError(t, st.CreateMonitor(ctx, m))
	require.NoError(t, st.SetDeliveryBudget(ctx, m.ID, intp(2)))
	for i := 0; i < 3; i++ {
		_, err := st.ConsumeDelivery(ctx, m.ID, budgetDay)
		require.NoError(t, err)
	}
	st.Close()

	reopened, err := NewSQLite(ctx, url)
	require.NoError(t, err)
	t.Cleanup(reopened.Close)
	d, err := reopened.ConsumeDelivery(ctx, m.ID, budgetDay)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "the budget was spent before the restart")
	assert.False(t, d.Announce, "and the exhaustion was already announced")
}
