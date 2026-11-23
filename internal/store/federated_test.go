package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// testFederatedAlerts covers the alert another SoroBeacon instance
// forwarded.
//
// The monitor and rule ids in the payload belong to the sending instance's
// database, so the stored alert must not be attached to any local row: it
// keeps the sender's ids as source ids, and its own monitor and rule ids are
// zero. The dedup key is the origin plus the event id, because the same
// event forwarded twice by one peer is one alert while the same event id
// from two peers is two.
func testFederatedAlerts(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	fed := &Alert{
		Federated:        true,
		FederationOrigin: "peer-a",
		SourceMonitorID:  42,
		SourceRuleID:     7,
		EventID:          "evt-1",
		Payload:          json.RawMessage(`{"contract_id":"C1"}`),
	}
	outcome, err := st.CreateAlert(ctx, fed)
	require.NoError(t, err)
	require.Equal(t, AlertCreated, outcome)

	got, err := st.GetAlert(ctx, fed.ID)
	require.NoError(t, err)
	assert.True(t, got.Federated, "the alert must be marked as federated")
	assert.Equal(t, "peer-a", got.FederationOrigin)
	assert.Equal(t, int64(42), got.SourceMonitorID, "the sender's monitor id is kept")
	assert.Equal(t, int64(7), got.SourceRuleID, "the sender's rule id is kept")
	assert.Zero(t, got.MonitorID, "a federated alert belongs to no local monitor")
	assert.Zero(t, got.RuleID, "a federated alert belongs to no local rule")

	// The same event from the same peer is one alert.
	outcome, err = st.CreateAlert(ctx, &Alert{
		Federated:        true,
		FederationOrigin: "peer-a",
		EventID:          "evt-1",
	})
	require.NoError(t, err)
	assert.Equal(t, AlertDuplicate, outcome)

	// The same event id from another peer is another alert.
	outcome, err = st.CreateAlert(ctx, &Alert{
		Federated:        true,
		FederationOrigin: "peer-b",
		EventID:          "evt-1",
	})
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, outcome)

	// A local alert is unaffected by the federated ones: the rule-keyed
	// guard still applies to it.
	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	rule := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, rule))
	local := &Alert{MonitorID: m.ID, RuleID: rule.ID, EventID: "evt-1"}
	outcome, err = st.CreateAlert(ctx, local)
	require.NoError(t, err)
	assert.Equal(t, AlertCreated, outcome,
		"a local alert and a federated one may share an event id")
}

// A federated alert lands in the workspace of the credential that delivered
// it, never in one the payload names: the payload is the other instance's
// data and cannot choose this instance's tenancy.
func testFederatedAlertFollowsTheDeliveringWorkspace(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	ctx := context.Background()

	acme := workspace.With(ctx, workspace.ID("acme"))
	beta := workspace.With(ctx, workspace.ID("beta"))

	fed := &Alert{
		Federated:        true,
		FederationOrigin: "peer-a",
		EventID:          "evt-acme",
	}
	outcome, err := st.CreateAlert(acme, fed)
	require.NoError(t, err)
	require.Equal(t, AlertCreated, outcome)

	acmeAlerts, err := st.ListAlerts(acme, AlertFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, acmeAlerts, 1)
	assert.Equal(t, fed.ID, acmeAlerts[0].ID)

	betaAlerts, err := st.ListAlerts(beta, AlertFilter{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, betaAlerts, "another workspace must not see the alert")
}
