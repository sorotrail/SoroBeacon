package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEscalationPolicyCRUD(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	chA := &Channel{Name: "a", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	chB := &Channel{Name: "b", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, chA))
	require.NoError(t, st.CreateChannel(ctx, chB))

	// A monitor with no policy fans out flat, so lookup is ErrNotFound.
	_, err := st.GetEscalationPolicyForMonitor(ctx, m.ID)
	require.ErrorIs(t, err, ErrNotFound)

	pol, err := st.SetEscalationPolicy(ctx, m.ID, []EscalationStep{
		{DelaySeconds: 0, ChannelIDs: []int64{chA.ID}},
		{DelaySeconds: 60, ChannelIDs: []int64{chB.ID}},
	})
	require.NoError(t, err)
	require.Len(t, pol.Steps, 2)
	assert.Equal(t, 0, pol.Steps[0].Position)
	assert.Equal(t, int64(60), pol.Steps[1].DelaySeconds)
	assert.Equal(t, []int64{chB.ID}, pol.Steps[1].ChannelIDs)

	got, err := st.GetEscalationPolicyForMonitor(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, pol.ID, got.ID)
	require.Len(t, got.Steps, 2)
	assert.Equal(t, []int64{chA.ID}, got.Steps[0].ChannelIDs)

	byID, err := st.GetEscalationPolicy(ctx, pol.ID)
	require.NoError(t, err)
	assert.Equal(t, m.ID, byID.MonitorID)

	// Replacing rewrites positions densely and drops the removed channel.
	pol2, err := st.SetEscalationPolicy(ctx, m.ID, []EscalationStep{{ChannelIDs: []int64{chA.ID}}})
	require.NoError(t, err)
	assert.Equal(t, pol.ID, pol2.ID, "the policy id is stable across a replace")
	require.Len(t, pol2.Steps, 1)
	assert.Equal(t, 0, pol2.Steps[0].Position)
	assert.Equal(t, []int64{chA.ID}, pol2.Steps[0].ChannelIDs)

	require.NoError(t, st.DeleteEscalationPolicy(ctx, m.ID))
	_, err = st.GetEscalationPolicyForMonitor(ctx, m.ID)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, st.DeleteEscalationPolicy(ctx, m.ID), ErrNotFound)
}

// TestDeleteChannelInUse pins the 409 path: a channel an escalation step
// references cannot be deleted until the policy lets go of it.
func TestDeleteChannelInUse(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	ch := &Channel{Name: "a", Type: "webhook", Config: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, ch))
	_, err := st.SetEscalationPolicy(ctx, m.ID, []EscalationStep{{ChannelIDs: []int64{ch.ID}}})
	require.NoError(t, err)

	require.ErrorIs(t, st.DeleteChannel(ctx, ch.ID), ErrChannelInUse)

	require.NoError(t, st.DeleteEscalationPolicy(ctx, m.ID))
	require.NoError(t, st.DeleteChannel(ctx, ch.ID))
}

// TestEscalationSchedulingAndAcknowledgement covers the persisted scheduling
// state: due steps are returned, not-yet-due ones are not, and acknowledging
// the alert removes it from the due set.
func TestEscalationSchedulingAndAcknowledgement(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	alert := &Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-1"}
	outcome, err := st.CreateAlert(ctx, alert)
	require.NoError(t, err)
	require.Equal(t, AlertCreated, outcome)

	pol, err := st.SetEscalationPolicy(ctx, m.ID, []EscalationStep{{}, {DelaySeconds: 300}})
	require.NoError(t, err)

	due := time.Now().Add(-time.Second).UTC()
	require.NoError(t, st.ScheduleEscalation(ctx, alert.ID, pol.ID, json.RawMessage(`{"id":1}`), 1, due))

	runs, err := st.DueEscalations(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, alert.ID, runs[0].AlertID)
	assert.Equal(t, 1, runs[0].NextStep)
	assert.JSONEq(t, `{"id":1}`, string(runs[0].Snapshot))

	// A future next-due time is not due yet.
	require.NoError(t, st.AdvanceEscalation(ctx, alert.ID, 2, time.Now().Add(time.Hour).UTC()))
	runs, err = st.DueEscalations(ctx, time.Now(), 10)
	require.NoError(t, err)
	assert.Empty(t, runs)

	// Acknowledging stops the escalation even though it is due again.
	require.NoError(t, st.AdvanceEscalation(ctx, alert.ID, 1, due))
	require.NoError(t, st.AcknowledgeAlert(ctx, alert.ID))
	runs, err = st.DueEscalations(ctx, time.Now(), 10)
	require.NoError(t, err)
	assert.Empty(t, runs, "an acknowledged alert must not escalate")

	got, err := st.GetAlert(ctx, alert.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AcknowledgedAt)

	// Idempotent, and a missing alert is not found.
	require.NoError(t, st.AcknowledgeAlert(ctx, alert.ID))
	require.ErrorIs(t, st.AcknowledgeAlert(ctx, alert.ID+9999), ErrNotFound)
}
