package poller

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// namedRule is a rule whose EventNames can answer, so WatchesFor may narrow
// the source's topic filter.
func namedRule(eventName string) store.Rule {
	return store.Rule{
		ID: 1, Type: rules.TypeEventEmitted, Enabled: true,
		Params: json.RawMessage(`{"event_name":"` + eventName + `"}`),
	}
}

// unnamedRule matches any event name, so the watch must stay unfiltered.
func unnamedRule() store.Rule {
	return store.Rule{
		ID: 2, Type: rules.TypeEventEmitted, Enabled: true,
		Params: json.RawMessage(`{"topic_equals":{"0":"transfer"}}`),
	}
}

// TestWatchesForNarrowsWhenEveryRuleNamesItsEvents: rules that all name
// their events produce one topic filter per name, in a stable order, so the
// backfill and the poller ask the RPC for the same events.
func TestWatchesForNarrowsWhenEveryRuleNamesItsEvents(t *testing.T) {
	reg := rules.NewRegistry()
	ruleList := []store.Rule{namedRule("transfer"), namedRule("mint")}

	watch := WatchesFor(reg, ruleList, []string{contractA})
	require.Len(t, watch, 1)
	require.Equal(t, contractA, watch[0].ContractID)

	want := [][]string{}
	for _, name := range []string{"mint", "transfer"} { // sorted: the union is order-stable
		filter, err := stellar.SymbolTopicFilter(name)
		require.NoError(t, err)
		want = append(want, filter)
	}
	assert.Equal(t, want, watch[0].Topics,
		"the filter must hold every name the rules match, encoded the way the RPC expects")
}

// TestWatchesForNarrowsOnlyWhenEveryRuleNamesItsEvents: rules that all
// name their events produce a topic filter, one rule that may match
// unnamed events forces the whole contract open, and a mix resolves to the
// unnamed rule — a server-side filter is only safe when it cannot exclude
// anything a rule might match.
func TestWatchesForNarrowsOnlyWhenEveryRuleNamesItsEvents(t *testing.T) {
	reg := rules.NewRegistry()

	transferFilter, err := stellar.SymbolTopicFilter("transfer")
	require.NoError(t, err)

	for _, tt := range []struct {
		name  string
		rules []store.Rule
		want  [][]string // nil means unfiltered
	}{
		{"one rule with names narrows", []store.Rule{namedRule("transfer")}, [][]string{transferFilter}},
		{"one rule without names stays open", []store.Rule{unnamedRule()}, nil},
		{"a mix: the unnamed rule wins", []store.Rule{namedRule("transfer"), unnamedRule()}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			watch := WatchesFor(reg, tt.rules, []string{contractA})
			require.Len(t, watch, 1)
			assert.Equal(t, tt.want, watch[0].Topics)
		})
	}
}

// TestWatchesForDropsInvalidContracts: one bad contract id would make the
// RPC reject the whole getEvents request, so it never reaches the source.
func TestWatchesForDropsInvalidContracts(t *testing.T) {
	reg := rules.NewRegistry()
	watch := WatchesFor(reg, []store.Rule{namedRule("transfer")},
		[]string{"not-a-contract", contractA, ""})
	require.Len(t, watch, 1)
	assert.Equal(t, contractA, watch[0].ContractID)
}

// TestWatchesForFallsBackWhenNamesExceedTheFilterCap: the RPC caps the topic
// filters within one getEvents filter, and a contract whose rules name more
// distinct events than that falls back to an unfiltered request rather than
// silently dropping the events a truncated filter would exclude.
func TestWatchesForFallsBackWhenNamesExceedTheFilterCap(t *testing.T) {
	reg := rules.NewRegistry()

	ruleList := []store.Rule{}
	for i := 0; i <= maxTopicFilters; i++ {
		ruleList = append(ruleList, namedRule(fmt.Sprintf("event_%d", i)))
	}
	watch := WatchesFor(reg, ruleList, []string{contractA})
	require.Len(t, watch, 1)
	assert.Nil(t, watch[0].Topics,
		"%d names exceed the cap of %d; narrowing would drop the ones that do not fit",
		maxTopicFilters+1, maxTopicFilters)

	// One below the cap still fits.
	fitting := ruleList[:maxTopicFilters]
	watch = WatchesFor(reg, fitting, []string{contractA})
	require.Len(t, watch, 1)
	require.Len(t, watch[0].Topics, maxTopicFilters,
		"%d names fit within the cap of %d", maxTopicFilters, maxTopicFilters)
}

// TestTopicFiltersFor pins the conversion's guard rails directly: empty is
// nothing to filter on, expressible names become one OR-ed filter each, and
// anything outside the cap is nil ("do not filter").
func TestTopicFiltersFor(t *testing.T) {
	assert.Nil(t, topicFiltersFor(nil), "no names means no filter")

	one, err := stellar.SymbolTopicFilter("transfer")
	require.NoError(t, err)
	assert.Equal(t, [][]string{one}, topicFiltersFor([]string{"transfer"}))

	tooMany := make([]string, 0, maxTopicFilters+1)
	for i := 0; i <= maxTopicFilters; i++ {
		tooMany = append(tooMany, fmt.Sprintf("event_%d", i))
	}
	assert.Nil(t, topicFiltersFor(tooMany))
}
