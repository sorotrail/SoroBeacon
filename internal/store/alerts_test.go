package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgresAlerts pins alert persistence and listing on the real Postgres
// schema: CreateAlert round-trips a payload, ListAlerts applies its monitor,
// rule and contract filters, honours both allowlisted sort orders (including
// the (created_at, id) boundary where two alerts share a timestamp), and its
// keyset pagination walks a full result set without repeating or skipping a
// row. Like the conformance suite it runs against TEST_DATABASE_URL and skips
// when it is unset, so `go test ./...` works without a database.
func TestPostgresAlerts(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres alert tests")
	}

	url := os.Getenv("TEST_DATABASE_URL")
	require.NoError(t, Migrate(url))
	st, err := NewPostgres(context.Background(), url, PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)
	require.NoError(t, st.resetConformance(context.Background()))
	ctx := context.Background()

	// Two monitors and two rules so the monitor/rule filters have rows they
	// must exclude, not just rows they happen to keep.
	mA := &Monitor{Name: "alerts-a", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, mA))
	mB := &Monitor{Name: "alerts-b", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, mB))
	rA := &Rule{MonitorID: mA.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, rA))
	rB := &Rule{MonitorID: mB.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, rB))

	// createAlert inserts one alert carrying contract_id in its payload, the
	// field the contract filter matches on, and returns the stored row.
	createAlert := func(t *testing.T, m *Monitor, r *Rule, eventID, contractID string) *Alert {
		t.Helper()
		a := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: eventID, Payload: json.RawMessage(`{"contract_id":"` + contractID + `"}`)}
		outcome, err := st.CreateAlert(ctx, a)
		require.NoError(t, err)
		require.Equal(t, AlertCreated, outcome)
		return a
	}

	// The boundary pair shares one created_at, which is what makes the sort
	// and keyset tests exercise the (created_at, id) tie-break rather than
	// timestamps that happen to differ.
	sameTime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	a1 := createAlert(t, mA, rA, "ev-1", "CA")
	a2 := createAlert(t, mA, rA, "ev-2", "CB")
	a3 := createAlert(t, mB, rB, "ev-3", "CB")
	a4 := createAlert(t, mA, rA, "ev-4", "CA")
	a5 := createAlert(t, mB, rB, "ev-5", "CA")
	for _, a := range []*Alert{a1, a2, a3, a4, a5} {
		require.NoError(t, st.setAlertCreatedAt(ctx, a.ID, sameTime))
	}

	// One more alert backdated past the shared timestamp. Its id is the
	// highest but its created_at is the oldest, so id order and (created_at,
	// id) order disagree — exactly the rows an id-only keyset cursor skips
	// or repeats, which is why the cursor must compare the full pair.
	a6 := createAlert(t, mA, rA, "ev-6", "CA")
	require.NoError(t, st.setAlertCreatedAt(ctx, a6.ID, sameTime.Add(-time.Hour)))

	t.Run("insert and read back with payload intact", func(t *testing.T) {
		got, err := st.GetAlert(ctx, a1.ID)
		require.NoError(t, err)
		assert.Equal(t, a1.ID, got.ID)
		assert.Equal(t, mA.ID, got.MonitorID)
		assert.Equal(t, rA.ID, got.RuleID)
		assert.Equal(t, "ev-1", got.EventID)
		assert.JSONEq(t, `{"contract_id":"CA"}`, string(got.Payload), "the stored payload must round-trip")
	})

	t.Run("list filters by monitor", func(t *testing.T) {
		list, err := st.ListAlerts(ctx, AlertFilter{MonitorID: mA.ID})
		require.NoError(t, err)
		require.Len(t, list, 4, "monitor A fired ev-1, ev-2, ev-4 and ev-6")
		for _, a := range list {
			assert.Equal(t, mA.ID, a.MonitorID)
		}
	})

	t.Run("list filters by rule", func(t *testing.T) {
		list, err := st.ListAlerts(ctx, AlertFilter{RuleID: rB.ID})
		require.NoError(t, err)
		require.Len(t, list, 2, "rule B fired ev-3 and ev-5")
		for _, a := range list {
			assert.Equal(t, rB.ID, a.RuleID)
		}
	})

	t.Run("list filters by payload contract id", func(t *testing.T) {
		list, err := st.ListAlerts(ctx, AlertFilter{ContractID: "CB"})
		require.NoError(t, err)
		require.Len(t, list, 2, "only ev-2 and ev-3 carry contract CB")
		seen := map[string]bool{}
		for _, a := range list {
			seen[a.EventID] = true
		}
		assert.Equal(t, map[string]bool{"ev-2": true, "ev-3": true}, seen)
	})

	t.Run("sort orders and the shared-timestamp boundary", func(t *testing.T) {
		desc, err := st.ListAlerts(ctx, AlertFilter{})
		require.NoError(t, err)
		require.Len(t, desc, 6)
		assert.Equal(t, []string{"ev-5", "ev-4", "ev-3", "ev-2", "ev-1", "ev-6"},
			eventIDs(desc),
			"created_at_desc orders by the (created_at, id) pair: the shared-timestamp group by descending id, then the older backdated row")

		asc, err := st.ListAlerts(ctx, AlertFilter{Sort: "created_at_asc"})
		require.NoError(t, err)
		require.Len(t, asc, 6)
		assert.Equal(t, []string{"ev-6", "ev-1", "ev-2", "ev-3", "ev-4", "ev-5"},
			eventIDs(asc),
			"created_at_asc orders by the (created_at, id) pair: the older backdated row first, then the shared-timestamp group by ascending id")

		// An unknown sort must fall back to the default rather than change
		// the ORDER BY shape, so a client typo cannot reorder pages.
		fallback, err := st.ListAlerts(ctx, AlertFilter{Sort: "not-a-sort"})
		require.NoError(t, err)
		require.Len(t, fallback, 6)
		assert.Equal(t, desc[0].ID, fallback[0].ID, "unknown sort falls back to created_at_desc")
	})

	t.Run("keyset pagination neither repeats nor skips rows", func(t *testing.T) {
		// Walk the whole set in pages of two, in both directions, and check
		// the property across pages rather than assuming any single page.
		walk := func(sort string, wantOrder []string) {
			var seen []string
			var after int64
			for page := 0; ; page++ {
				got, err := st.ListAlerts(ctx, AlertFilter{Sort: sort, AfterID: after, Limit: 2})
				require.NoError(t, err)
				for _, a := range got {
					seen = append(seen, a.EventID)
					after = a.ID
				}
				if len(got) < 2 {
					break
				}
				require.Less(t, page, 10, "pagination did not terminate")
			}
			assert.Equal(t, wantOrder, seen,
				"pages must cover every alert exactly once, in the sort's order")
		}
		walk("created_at_desc", []string{"ev-5", "ev-4", "ev-3", "ev-2", "ev-1", "ev-6"})
		walk("created_at_asc", []string{"ev-6", "ev-1", "ev-2", "ev-3", "ev-4", "ev-5"})
	})

	t.Run("empty result set", func(t *testing.T) {
		list, err := st.ListAlerts(ctx, AlertFilter{ContractID: "no-such-contract"})
		require.NoError(t, err)
		assert.Empty(t, list, "a filter matching nothing must return an empty, non-nil result")
		assert.NotNil(t, list)
	})
}

// eventIDs maps a listing to its event ids, so order assertions read as the
// scenario they pin instead of a column of indexes.
func eventIDs(alerts []Alert) []string {
	ids := make([]string, len(alerts))
	for i, a := range alerts {
		ids[i] = a.EventID
	}
	return ids
}
