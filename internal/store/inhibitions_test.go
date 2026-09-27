package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestInhibitionStore exercises the inhibition persistence against both
// backends: pair CRUD, the firing signal, and the suppression mark. The pure
// decision lives in internal/alerts (see inhibit_test.go); this file proves
// the SQL behind it on Postgres and SQLite alike.
func TestInhibitionStore(t *testing.T) {
	t.Run("Postgres", func(t *testing.T) {
		if os.Getenv("TEST_DATABASE_URL") == "" {
			t.Skip("TEST_DATABASE_URL not set; skipping Postgres inhibition tests")
		}
		testInhibitionStore(t, newTestPostgres(t))
	})
	t.Run("SQLite", func(t *testing.T) {
		testInhibitionStore(t, newTestSQLite(t))
	})
}

func testInhibitionStore(t *testing.T, st conformanceStore) {
	t.Helper()
	ctx := context.Background()

	setup := func(t *testing.T) (sourceID, targetID, alertID int64) {
		t.Helper()
		m := &Monitor{Name: "inhib", ContractIDs: []string{"CABC"}, Enabled: true}
		require.NoError(t, st.CreateMonitor(ctx, m))
		source := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: []byte(`{"event_name":"transfer"}`), Enabled: true}
		target := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: []byte(`{"event_name":"mint"}`), Enabled: true}
		require.NoError(t, st.CreateRule(ctx, source))
		require.NoError(t, st.CreateRule(ctx, target))
		alert := &Alert{
			MonitorID: m.ID, RuleID: source.ID, EventID: "ev-1",
			Payload: []byte(`{"contract_id":"CABC"}`),
		}
		outcome, err := st.CreateAlert(ctx, alert)
		require.NoError(t, err)
		require.Equal(t, AlertCreated, outcome)
		return source.ID, target.ID, alert.ID
	}

	t.Run("CreateListDelete", func(t *testing.T) {
		sourceID, targetID, _ := setup(t)
		in := &Inhibition{SourceRuleID: sourceID, TargetRuleID: targetID, FiringWindowSeconds: 60}
		require.NoError(t, st.CreateInhibition(ctx, in))
		require.False(t, in.CreatedAt.IsZero())

		all, err := st.ListInhibitions(ctx)
		require.NoError(t, err)
		require.Len(t, all, 1)
		require.Equal(t, 60, all[0].FiringWindowSeconds)

		forTarget, err := st.ListInhibitionsForTarget(ctx, targetID)
		require.NoError(t, err)
		require.Len(t, forTarget, 1)

		forSource, err := st.ListInhibitionsForTarget(ctx, sourceID)
		require.NoError(t, err)
		require.Empty(t, forSource, "the source side is not a target")

		require.NoError(t, st.DeleteInhibition(ctx, sourceID, targetID))
		require.ErrorIs(t, st.DeleteInhibition(ctx, sourceID, targetID), ErrNotFound)
	})

	t.Run("DefaultWindow", func(t *testing.T) {
		sourceID, targetID, _ := setup(t)
		in := &Inhibition{SourceRuleID: sourceID, TargetRuleID: targetID}
		require.NoError(t, st.CreateInhibition(ctx, in))
		require.Equal(t, DefaultInhibitionWindowSeconds, in.FiringWindowSeconds)
		require.NoError(t, st.DeleteInhibition(ctx, sourceID, targetID))
	})

	t.Run("DuplicateAndMissingRule", func(t *testing.T) {
		sourceID, targetID, _ := setup(t)
		require.NoError(t, st.CreateInhibition(ctx,
			&Inhibition{SourceRuleID: sourceID, TargetRuleID: targetID}))
		// Same pair twice violates the primary key.
		require.Error(t, st.CreateInhibition(ctx,
			&Inhibition{SourceRuleID: sourceID, TargetRuleID: targetID}))
		// A missing rule violates the foreign key.
		require.Error(t, st.CreateInhibition(ctx,
			&Inhibition{SourceRuleID: 424242, TargetRuleID: targetID}))
		require.NoError(t, st.DeleteInhibition(ctx, sourceID, targetID))
	})

	t.Run("FiringSignalAndSuppressionMark", func(t *testing.T) {
		sourceID, targetID, alertID := setup(t)

		fired, err := st.RuleFiredWithin(ctx, sourceID, time.Minute)
		require.NoError(t, err)
		require.True(t, fired, "the source just produced an alert")

		fired, err = st.RuleFiredWithin(ctx, targetID, time.Minute)
		require.NoError(t, err)
		require.False(t, fired, "the target produced nothing")

		require.NoError(t, st.MarkAlertInhibited(ctx, alertID, sourceID))
		got, err := st.GetAlert(ctx, alertID)
		require.NoError(t, err)
		require.NotNil(t, got.InhibitedByRuleID)
		require.Equal(t, sourceID, *got.InhibitedByRuleID)
	})
}
