package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The params fixtures are written in jsonb's own canonical text form — keys
// sorted shortest-first, ": " and ", " separators — so the byte-for-byte
// comparisons below measure the store, not jsonb's normalisation. The list
// fixture nests an object: params are JSON the database never validates, so
// a round trip that only survives flat blobs proves little.
const (
	paramsList    = `{"window": "1h", "threshold": {"max": 10, "min": 1}}`
	paramsUpdated = `{"note": "low-volume", "window": "6h", "threshold": {"max": 5}}`
)

// assertParamsEqual compares params behind a boolean: the point of the round
// trip is byte equality, and a plain Equal would print the whole blob when it
// fails instead of naming the assertion that broke.
func assertParamsEqual(t *testing.T, want string, got json.RawMessage, msg string) {
	t.Helper()
	assert.True(t, bytes.Equal([]byte(want), got), msg)
}

// ruleIDs narrows rules to their ids so list assertions report ids instead
// of dumping the rows and their params.
func ruleIDs(rs []Rule) []int64 {
	ids := make([]int64, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	return ids
}

// TestRuleCreateReadParamsRoundTrip pins create → read: the rule comes back
// under the right monitor with its type and enabled flag intact, and its
// params survive byte-for-byte — the rule registry validates them at
// evaluation time, so the store must pass them through unchanged.
func TestRuleCreateReadParamsRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))

	r := &Rule{
		MonitorID: m.ID,
		Type:      "value_threshold",
		Params:    json.RawMessage(paramsList),
		Enabled:   true,
	}
	require.NoError(t, st.CreateRule(ctx, r))
	require.NotZero(t, r.ID, "create must fill the new id")

	got, err := st.GetRule(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, r.ID, got.ID)
	assert.Equal(t, m.ID, got.MonitorID)
	assert.Equal(t, "value_threshold", got.Type)
	assert.True(t, got.Enabled)
	assertParamsEqual(t, paramsList, got.Params,
		"params must round-trip byte-for-byte, nested JSON included")
}

// TestRuleListForMonitor covers both sides of the listing the poller and
// dashboard call: a monitor's rules come back in id order and only its own,
// while a monitor without rules gets an empty list rather than an error.
func TestRuleListForMonitor(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m1 := &Monitor{Name: "m1", ContractIDs: []string{"CAAA"}, Enabled: true}
	m2 := &Monitor{Name: "m2", ContractIDs: []string{"CBBB"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m1))
	require.NoError(t, st.CreateMonitor(ctx, m2))

	r1 := &Rule{MonitorID: m1.ID, Type: "event_emitted", Params: json.RawMessage(paramsList), Enabled: true}
	r2 := &Rule{MonitorID: m1.ID, Type: "value_threshold", Params: json.RawMessage(`{}`), Enabled: false}
	require.NoError(t, st.CreateRule(ctx, r1))
	require.NoError(t, st.CreateRule(ctx, r2))

	list, err := st.ListRules(ctx, m1.ID, false)
	require.NoError(t, err)
	require.Len(t, ruleIDs(list), 2, "both of the monitor's rules are listed")
	assert.Equal(t, []int64{r1.ID, r2.ID}, ruleIDs(list),
		"a monitor's rules are listed in id order, and only its own")
	for _, r := range list {
		assert.Equal(t, m1.ID, r.MonitorID, "no other monitor's rules may appear")
	}
	assertParamsEqual(t, paramsList, list[0].Params, "the list path must read params intact too")

	empty, err := st.ListRules(ctx, m2.ID, false)
	require.NoError(t, err)
	assert.Empty(t, ruleIDs(empty), "a monitor without rules lists empty, not an error")
}

// TestRuleUpdateParamsAndEnabled covers the columns a rule edit touches: new
// params must replace the stored blob byte-for-byte, and the enabled flag
// must stick in both directions because the poller's enabled-only listing
// reads it.
func TestRuleUpdateParamsAndEnabled(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))

	r := &Rule{MonitorID: m.ID, Type: "value_threshold", Params: json.RawMessage(paramsList), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))

	r.Params = json.RawMessage(paramsUpdated)
	r.Enabled = false
	require.NoError(t, st.UpdateRule(ctx, r))

	got, err := st.GetRule(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, m.ID, got.MonitorID, "an update must not move the rule")
	assert.Equal(t, "value_threshold", got.Type)
	assert.False(t, got.Enabled, "disabling must persist")
	assertParamsEqual(t, paramsUpdated, got.Params, "updated params must replace the stored blob")

	r.Enabled = true
	require.NoError(t, st.UpdateRule(ctx, r))
	got, err = st.GetRule(ctx, r.ID)
	require.NoError(t, err)
	assert.True(t, got.Enabled, "re-enabling must persist too")
}

// TestRuleDeleteLeavesSiblings covers the opposite direction of the monitor
// cascade in 0001_init: rules cascade away with their monitor, but deleting
// one rule must remove exactly that row — the other rules on the monitor
// keep their params and enabled flags.
func TestRuleDeleteLeavesSiblings(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))

	keep1 := &Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(paramsList), Enabled: true}
	doomed := &Rule{MonitorID: m.ID, Type: "value_threshold", Params: json.RawMessage(`{}`), Enabled: false}
	keep2 := &Rule{MonitorID: m.ID, Type: "frequency", Params: json.RawMessage(`{}`), Enabled: false}
	require.NoError(t, st.CreateRule(ctx, keep1))
	require.NoError(t, st.CreateRule(ctx, doomed))
	require.NoError(t, st.CreateRule(ctx, keep2))

	require.NoError(t, st.DeleteRule(ctx, doomed.ID))

	_, err := st.GetRule(ctx, doomed.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	list, err := st.ListRules(ctx, m.ID, false)
	require.NoError(t, err)
	require.Len(t, ruleIDs(list), 2, "only the deleted rule may disappear")
	assert.Equal(t, []int64{keep1.ID, keep2.ID}, ruleIDs(list),
		"the surviving siblings, in id order")
	assert.True(t, list[0].Enabled, "a sibling keeps its enabled flag")
	assert.False(t, list[1].Enabled, "a sibling keeps its disabled flag")
	assertParamsEqual(t, paramsList, list[0].Params, "a sibling keeps its params")
}

// TestRuleCreateUnknownMonitor asserts what the store actually does with a
// dangling monitor id: the foreign key rejects the insert and mapErr turns
// that into ErrNotFound, so callers answer 404 instead of surfacing a
// driver error, and no id is handed back.
func TestRuleCreateUnknownMonitor(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	m := &Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))

	r := &Rule{
		MonitorID: m.ID + 1000000,
		Type:      "event_emitted",
		Params:    json.RawMessage(paramsList),
		Enabled:   true,
	}
	err := st.CreateRule(ctx, r)
	require.Error(t, err, "a rule without its monitor must be rejected")
	assert.ErrorIs(t, err, ErrNotFound, "the FK violation maps to ErrNotFound")
	assert.Zero(t, r.ID, "a rejected insert must not fill in an id")
}
