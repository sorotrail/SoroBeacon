package poller

import (
	"sort"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// WatchesFor derives the source watch list for one monitor: its valid
// contracts plus, when every enabled rule names a concrete event, the shared
// topic filter. It mirrors what the live poller sends each cycle, factored out
// so a backfill queries exactly the events the poller would have for the same
// monitor. Invalid contract IDs are dropped (the RPC rejects the whole request
// for one bad ID); Topics stays nil when any rule may match unnamed events,
// which means "do not narrow" — always correct, if less efficient.
func WatchesFor(reg *rules.Registry, ruleList []store.Rule, contracts []string) []Watch {
	var topics [][]string
	if names, ok := ruleEventNames(reg, ruleList); ok {
		topics = topicFiltersFor(names)
	}
	watch := make([]Watch, 0, len(contracts))
	for _, c := range contracts {
		if !stellar.IsValidContractID(c) {
			continue
		}
		watch = append(watch, Watch{ContractID: c, Topics: topics})
	}
	return watch
}

// maxTopicFilters is the RPC's cap on topic filters within one getEvents
// filter. A contract whose rules name more distinct events than this cannot be
// expressed, so it falls back to an unfiltered request rather than silently
// dropping the events a truncated filter would exclude.
const maxTopicFilters = 5

// ruleEventNames returns the union of the concrete event names the enabled
// rules match. ok is false when any rule may match unnamed events, in which
// case the whole contract must stay unfiltered: a server-side filter is only
// safe when it cannot exclude anything a rule might match.
func ruleEventNames(reg *rules.Registry, ruleList []store.Rule) ([]string, bool) {
	seen := map[string]bool{}
	for _, rule := range ruleList {
		names, ok := reg.EventNames(rule.Type, rule.Params)
		if !ok {
			return nil, false
		}
		for _, n := range names {
			seen[n] = true
		}
	}
	return sortedKeys(seen), true
}

// topicFiltersFor turns a set of event names into getEvents topic filters,
// returning nil when they cannot be expressed safely. nil means "do not
// filter", which is always correct, if less efficient.
func topicFiltersFor(names []string) [][]string {
	if len(names) == 0 || len(names) > maxTopicFilters {
		return nil
	}
	out := make([][]string, 0, len(names))
	for _, name := range names {
		filter, err := stellar.SymbolTopicFilter(name)
		if err != nil {
			return nil
		}
		out = append(out, filter)
	}
	return out
}

// sortedKeys returns the set's keys in a stable order, so a given rule set
// always produces the same filter list (and therefore the same test output).
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
