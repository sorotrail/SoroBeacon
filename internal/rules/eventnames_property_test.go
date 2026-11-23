package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// eventNamesSafetyCases are params whose EventNames answer must cover every
// event the evaluator can match: the poller compiles server-side getEvents
// topic filters from that answer, and a name the answer drops is an event
// the RPC never sends — the rule silently stops firing with nothing in the
// alerting path to notice.
var eventNamesSafetyCases = []struct {
	name   string
	typ    string
	params string
}{
	{"event_emitted scoped by name", TypeEventEmitted, `{"event_name":"transfer"}`},
	{"value_threshold scoped by name", TypeValueThreshold, `{"event_name":"mint","comparison":"gt","threshold":1}`},
	{"token_event one name", TypeTokenEvent, `{"event":"burn"}`},
	{"token_event wildcard expands to every SEP-41 event", TypeTokenEvent, `{"event":"*"}`},
	{"token_supply_change mint only", TypeTokenSupplyChange, `{"direction":"mint"}`},
	{"token_supply_change any covers both", TypeTokenSupplyChange, `{"direction":"any"}`},
	{"frequency_threshold scoped by name", TypeFrequencyThreshold, `{"event_name":"swap","count":1,"window":"1m"}`},
	{"topic_position scoped by name", TypeTopicPosition,
		`{"position":1,"equals":"` + wlAlice + `","event":"deposit"}`},
	{"address_watchlist restricted to one event", TypeAddressWatchlist,
		`{"addresses":["` + wlAlice + `"],"event":"clawback"}`},
	{"address_watchlist unrestricted covers every SEP-41 event", TypeAddressWatchlist,
		`{"addresses":["` + wlAlice + `"]}`},
}

// eventNamesSafetyUniverse is one event per candidate name, shaped so each
// evaluator's own checks pass: three topics with symbol/address wrappers and
// a plain numeric value (value_threshold parses strings, token_event needs
// no amount filter here). The names inside a case's returned set and the
// names outside it all come from this one pool, so "outside" always has
// representatives.
func eventNamesSafetyUniverse() map[string]*stellar.DecodedEvent {
	names := []string{
		// The SEP-41 names token_event and token_supply_change care about.
		"transfer", "mint", "burn", "clawback", "set_admin",
		// Non-SEP-41 names, so "outside" includes events the layout does not
		// know at all.
		"swap", "deposit", "vote",
	}
	universe := make(map[string]*stellar.DecodedEvent, len(names))
	for _, n := range names {
		universe[n] = sep41Event(n, wlAlice, wlBob, "500")
	}
	return universe
}

// TestEventNamesSafetyProperty is the safety property behind topic narrowing:
// for every evaluator and params shape that names its events, no event whose
// name is outside the returned set may match. If an EventNames
// implementation drops a name it can match, the poller asks the RPC for a
// subset of what the rule needs and the dropped events never arrive — the
// rule stops firing silently, and no alert can ever say why.
//
// The test fails if an implementation drops a name: temporarily return a
// subset from any EventNames method (or from Registry.EventNames) and every
// case scoped to that evaluator fails here.
func TestEventNamesSafetyProperty(t *testing.T) {
	reg := NewRegistry()
	universe := eventNamesSafetyUniverse()

	for _, tc := range eventNamesSafetyCases {
		t.Run(tc.name, func(t *testing.T) {
			names, ok := reg.EventNames(tc.typ, json.RawMessage(tc.params))
			require.True(t, ok, "this case exists because the rule can name its events")

			inSet := map[string]bool{}
			for _, n := range names {
				inSet[n] = true
			}

			// The case must be meaningful: at least one event inside the
			// returned set actually matches, so the loop below is not
			// passing over an evaluator that never matches anything.
			matchedInside := false
			for name, ev := range universe {
				matched, err := reg.Evaluate(context.Background(), tc.typ, ev, json.RawMessage(tc.params))
				require.NoError(t, err, "event %q", name)

				if inSet[name] {
					if matched {
						matchedInside = true
					}
					continue
				}
				assert.False(t, matched,
					"%s matched event %q, which is outside EventNames' %v — a filter on those names would never fetch it",
					tc.typ, name, names)
			}
			require.True(t, matchedInside,
				"no event inside %v matched; the case proves nothing about the ones outside", names)
		})
	}
}

// TestRegistryEventNames pins where the registry refuses to narrow: an
// unknown type, a type whose evaluator does not implement EventNamer, and
// the absence type — none of them may produce names, because ok=false is
// what leaves the contract unfiltered (always correct, if less efficient).
func TestRegistryEventNames(t *testing.T) {
	reg := NewRegistry()

	t.Run("unknown type", func(t *testing.T) {
		got, ok := reg.EventNames("not_a_rule", json.RawMessage(`{}`))
		assert.False(t, ok)
		assert.Nil(t, got)
	})

	t.Run("absence rules are timer-driven and name nothing", func(t *testing.T) {
		got, ok := reg.EventNames(TypeAbsenceOfEvent,
			json.RawMessage(`{"event_name":"transfer","window":"5m"}`))
		assert.False(t, ok, "an absence rule matches by silence; a name would wrongly narrow the fetch")
		assert.Nil(t, got)
	})

	// Composite children are resolved through the registry at evaluation
	// time, but the composite itself does not implement EventNamer. An "or"
	// could union its children's names in principle, while a "not" can match
	// almost anything; today neither is attempted, and ok=false keeps both
	// cases unfiltered rather than half-filtered.
	t.Run("composite rules name nothing even when their children do", func(t *testing.T) {
		for _, params := range []string{
			`{"op":"or","rules":[` +
				`{"type":"event_emitted","params":{"event_name":"transfer"}},` +
				`{"type":"event_emitted","params":{"event_name":"mint"}}]}`,
			`{"op":"not","rules":[{"type":"event_emitted","params":{"event_name":"transfer"}}]}`,
		} {
			got, ok := reg.EventNames(TypeComposite, json.RawMessage(params))
			assert.False(t, ok, "params %s", params)
			assert.Nil(t, got, "params %s", params)
		}
	})

	// Evaluators that never implemented EventNamer must answer the same way
	// as one whose EventNames returned ok=false: no names, no narrowing.
	t.Run("types without an EventNamer", func(t *testing.T) {
		for _, typ := range []string{
			TypeNumericRange, TypeEventNameGlob, TypeContractAllowlist,
			TypeTopicRegex, TypeSelfTransfer, TypeTimeWindow,
		} {
			got, ok := reg.EventNames(typ, json.RawMessage(`{}`))
			assert.False(t, ok, "%s must not be narrowed", typ)
			assert.Nil(t, got, "%s", typ)
		}
	})

	t.Run("an evaluator registered without EventNamer", func(t *testing.T) {
		custom := NewRegistry()
		custom.Register("custom_plain", EventEmitted{})
		custom.Register("custom_not_an_event_namer", plainEvaluator{})
		got, ok := custom.EventNames("custom_plain", json.RawMessage(`{"event_name":"transfer"}`))
		assert.True(t, ok)
		assert.Equal(t, []string{"transfer"}, got)
		got, ok = custom.EventNames("custom_not_an_event_namer", json.RawMessage(`{}`))
		assert.False(t, ok)
		assert.Nil(t, got)
	})

	t.Run("narrowing answers of the built-in types", func(t *testing.T) {
		got, ok := reg.EventNames(TypeEventEmitted, json.RawMessage(`{"event_name":"transfer"}`))
		require.True(t, ok)
		assert.Equal(t, []string{"transfer"}, got)
		// A rule with no event_name judges every event: names would lie.
		got, ok = reg.EventNames(TypeEventEmitted, json.RawMessage(`{}`))
		assert.False(t, ok)
		assert.Nil(t, got)
	})
}

// plainEvaluator is a valid evaluator that does not implement EventNamer, so
// the registry has to fall back to "cannot narrow".
type plainEvaluator struct{}

func (plainEvaluator) Validate(json.RawMessage) error { return nil }

func (plainEvaluator) Evaluate(context.Context, *stellar.DecodedEvent, json.RawMessage) (bool, error) {
	return false, nil
}
