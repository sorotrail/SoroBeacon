package alerts

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestFlappingDetector(t *testing.T) {
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	t.Run("threshold boundary and time window table", func(t *testing.T) {
		cfg := FlappingConfig{
			Episodes:   3,
			Window:     15 * time.Minute,
			QuietAfter: 2 * time.Minute,
		}
		detector := NewFlappingDetector(cfg).WithClock(func() time.Time { return base })

		tests := []struct {
			name         string
			times        []time.Time
			wantFlapping bool
			wantEpisodes int
		}{
			{
				name:         "empty history is not flapping",
				times:        nil,
				wantFlapping: false,
				wantEpisodes: 0,
			},
			{
				name:         "single transition is not flapping",
				times:        []time.Time{base.Add(-1 * time.Minute)},
				wantFlapping: false,
				wantEpisodes: 1,
			},
			{
				name: "just under threshold (2 episodes) is not flapping",
				times: []time.Time{
					base.Add(-10 * time.Minute),
					base.Add(-5 * time.Minute),
				},
				wantFlapping: false,
				wantEpisodes: 2,
			},
			{
				name: "exactly at threshold (3 episodes) is flapping",
				times: []time.Time{
					base.Add(-10 * time.Minute),
					base.Add(-5 * time.Minute),
					base.Add(-1 * time.Minute),
				},
				wantFlapping: true,
				wantEpisodes: 3,
			},
			{
				name: "just over threshold (4 episodes) is flapping",
				times: []time.Time{
					base.Add(-13 * time.Minute),
					base.Add(-9 * time.Minute),
					base.Add(-5 * time.Minute),
					base.Add(-1 * time.Minute),
				},
				wantFlapping: true,
				wantEpisodes: 4,
			},
			{
				name: "transitions spread beyond the window (twice a day) are not flapping",
				times: []time.Time{
					base.Add(-36 * time.Hour),
					base.Add(-24 * time.Hour),
					base.Add(-12 * time.Hour),
					base.Add(-2 * time.Minute),
				},
				wantFlapping: false,
				wantEpisodes: 1,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.wantFlapping, detector.IsFlapping(tt.times))
				assert.Equal(t, tt.wantEpisodes, detector.Evaluate(tt.times).Episodes)
			})
		}
	})

	t.Run("zero or negative configuration falls back to defaults instead of disabling detection", func(t *testing.T) {
		for _, raw := range []FlappingConfig{
			{},
			{Episodes: -1, Window: -5 * time.Minute, QuietAfter: -1 * time.Minute},
		} {
			d := NewFlappingDetector(raw).WithClock(func() time.Time { return base })
			assert.Equal(t, FlappingConfig{
				Episodes:   DefaultFlappingEpisodes,
				Window:     DefaultFlappingWindow,
				QuietAfter: DefaultFlappingQuietAfter,
			}, d.Config())

			threeEpisodes := []time.Time{
				base.Add(-10 * time.Minute),
				base.Add(-5 * time.Minute),
				base.Add(-1 * time.Minute),
			}
			assert.True(t, d.IsFlapping(threeEpisodes), "default fallback must detect 3 episodes in 15m")
		}
	})
}

func openTestSQLiteForLifecycle(t *testing.T) *store.SQLite {
	t.Helper()
	resolutionMu.Lock()
	resolvedAlertMap = map[int64]resolutionRecord{}
	resolutionMu.Unlock()

	dbURL := "sqlite://" + filepath.Join(t.TempDir(), "lifecycle.db")
	require.NoError(t, store.Migrate(dbURL))
	st, err := store.NewSQLite(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	return st
}

func seedLifecycleAlertWithEscalation(t *testing.T, st store.Store, ctx context.Context, ws workspace.ID) (int64, int64) {
	t.Helper()
	sysCtx := workspace.WithSystem(ctx)
	wsCtx := workspace.With(ctx, ws)

	require.NoError(t, st.EnsureWorkspace(sysCtx, ws))

	ch := &store.Channel{
		Name:    "ch-" + string(ws),
		Type:    "webhook",
		Config:  json.RawMessage(`{"url":"https://example.com/hook","secret":"s"}`),
		Enabled: true,
	}
	require.NoError(t, st.CreateChannel(wsCtx, ch))

	mon := &store.Monitor{
		Name:        "mon-" + string(ws),
		ContractIDs: []string{"C" + string(ws)},
		Enabled:     true,
		ChannelIDs:  []int64{ch.ID},
	}
	require.NoError(t, st.CreateMonitor(wsCtx, mon))

	rule := &store.Rule{
		MonitorID: mon.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{}`),
		Enabled:   true,
	}
	require.NoError(t, st.CreateRule(wsCtx, rule))

	pol, err := st.SetEscalationPolicy(wsCtx, mon.ID, []store.EscalationStep{
		{DelaySeconds: 0, ChannelIDs: []int64{ch.ID}},
		{DelaySeconds: 60, ChannelIDs: []int64{ch.ID}},
	})
	require.NoError(t, err)

	alert := &store.Alert{
		MonitorID: mon.ID,
		RuleID:    rule.ID,
		EventID:   "evt-lifecycle-" + string(ws),
		Payload:   json.RawMessage(`{}`),
	}
	outcome, err := st.CreateAlert(wsCtx, alert)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	dueAt := time.Now().UTC().Add(-1 * time.Minute)
	require.NoError(t, st.ScheduleEscalation(sysCtx, alert.ID, pol.ID, json.RawMessage(`{"id":1}`), 1, dueAt))
	return alert.ID, pol.ID
}

func TestAlertLifecycleAcknowledgeAndResolve(t *testing.T) {
	ctx := context.Background()

	t.Run("Acknowledge marks open alert acknowledged, is idempotent, and stops escalation", func(t *testing.T) {
		st := openTestSQLiteForLifecycle(t)
		wsCtx := workspace.With(ctx, "alpha")
		sysCtx := workspace.WithSystem(ctx)

		alertID, _ := seedLifecycleAlertWithEscalation(t, st, ctx, "alpha")

		// Verify escalation is initially due.
		dueBefore, err := st.DueEscalations(sysCtx, time.Now().UTC(), 10)
		require.NoError(t, err)
		require.Len(t, dueBefore, 1)

		ack1, err := Acknowledge(wsCtx, st, alertID, "alice")
		require.NoError(t, err)
		require.NotNil(t, ack1.AcknowledgedAt)
		assert.Equal(t, "alice", ack1.AcknowledgedBy)

		// Escalation is stopped atomically in the store.
		dueAfter, err := st.DueEscalations(sysCtx, time.Now().UTC(), 10)
		require.NoError(t, err)
		assert.Empty(t, dueAfter, "acknowledging an alert must cancel pending escalations")

		// Idempotent second Acknowledge preserves first timestamp and actor.
		time.Sleep(10 * time.Millisecond)
		ack2, err := Acknowledge(wsCtx, st, alertID, "bob")
		require.NoError(t, err)
		require.NotNil(t, ack2.AcknowledgedAt)
		assert.True(t, ack1.AcknowledgedAt.Equal(*ack2.AcknowledgedAt), "acknowledging twice must preserve the first AcknowledgedAt timestamp")
		assert.Equal(t, "alice", ack2.AcknowledgedBy, "acknowledging twice must preserve the first AcknowledgedBy")
	})

	t.Run("Resolve resolves open or acknowledged alert, stops escalation, and is idempotent", func(t *testing.T) {
		st := openTestSQLiteForLifecycle(t)
		wsCtx := workspace.With(ctx, "alpha")
		sysCtx := workspace.WithSystem(ctx)

		alertID, _ := seedLifecycleAlertWithEscalation(t, st, ctx, "alpha")
		t1 := time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC)
		t2 := t1.Add(5 * time.Minute)

		res1, err := ResolveAt(wsCtx, st, alertID, "oncall-1", t1)
		require.NoError(t, err)
		gotResolved1 := ResolvedAt(res1.ID)
		require.NotNil(t, gotResolved1)
		assert.True(t, t1.Equal(*gotResolved1))
		require.NotNil(t, res1.AcknowledgedAt, "resolving an open alert also stamps acknowledgement so escalation stops")

		dueAfter, err := st.DueEscalations(sysCtx, time.Now().UTC(), 10)
		require.NoError(t, err)
		assert.Empty(t, dueAfter, "resolving an alert must stop pending escalations")

		// Resolving an already-resolved alert is idempotent and does not overwrite resolved_at.
		res2, err := ResolveAt(wsCtx, st, alertID, "oncall-2", t2)
		require.NoError(t, err)
		gotResolved2 := ResolvedAt(res2.ID)
		require.NotNil(t, gotResolved2)
		assert.True(t, t1.Equal(*gotResolved2), "resolving an already-resolved alert must keep original resolution time")

		// Acknowledging an already-resolved alert is a predictable no-op that keeps it resolved.
		ackAfterResolve, err := Acknowledge(wsCtx, st, alertID, "late-operator")
		require.NoError(t, err)
		assert.NotNil(t, ResolvedAt(ackAfterResolve.ID), "acknowledging a resolved alert must not reopen it")
	})

	t.Run("Resolving an acknowledged alert transitions cleanly to resolved", func(t *testing.T) {
		st := openTestSQLiteForLifecycle(t)
		wsCtx := workspace.With(ctx, "alpha")
		alertID, _ := seedLifecycleAlertWithEscalation(t, st, ctx, "alpha")

		ack, err := Acknowledge(wsCtx, st, alertID, "alice")
		require.NoError(t, err)
		require.NotNil(t, ack.AcknowledgedAt)
		assert.Nil(t, ResolvedAt(ack.ID))

		tResolve := time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)
		res, err := ResolveAt(wsCtx, st, alertID, "alice", tResolve)
		require.NoError(t, err)
		require.NotNil(t, ResolvedAt(res.ID))
		assert.True(t, tResolve.Equal(*ResolvedAt(res.ID)))
		assert.True(t, ack.AcknowledgedAt.Equal(*res.AcknowledgedAt))
	})

	t.Run("missing alert and cross-workspace isolation return store.ErrNotFound", func(t *testing.T) {
		st := openTestSQLiteForLifecycle(t)
		alphaCtx := workspace.With(ctx, "alpha")
		betaCtx := workspace.With(ctx, "beta")
		require.NoError(t, st.EnsureWorkspace(workspace.WithSystem(ctx), "beta"))

		alertID, _ := seedLifecycleAlertWithEscalation(t, st, ctx, "alpha")

		// Missing alert ID returns store.ErrNotFound.
		_, err := Acknowledge(alphaCtx, st, 999999, "alice")
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = Resolve(alphaCtx, st, 999999, "alice")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// Tenant beta cannot acknowledge or resolve tenant alpha's alert.
		_, err = Acknowledge(betaCtx, st, alertID, "mallory")
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = Resolve(betaCtx, st, alertID, "mallory")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// Verify alpha's alert remains untouched.
		got, err := st.GetAlert(alphaCtx, alertID)
		require.NoError(t, err)
		assert.Nil(t, got.AcknowledgedAt)
		assert.Nil(t, ResolvedAt(got.ID))
	})
}
