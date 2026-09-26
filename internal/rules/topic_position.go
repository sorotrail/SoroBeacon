package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// topicPositionParams configure the topic_position rule.
//
//	{
//	  "position": 2,                  // required: topic index to compare
//	  "equals": "POOL_USDC_XLM",      // required: exact value the topic must equal
//	  "event": "deposit"              // optional: also require this event name
//	}
//
// Custom (non-SEP-41) contracts put meaningful values in fixed topic
// positions — a pool ID, a market symbol, an account — and event_emitted can
// only look at the first topic while token_event only understands SEP-41's
// slots. This rule is the missing "position N equals V" question, so watching
// one pool does not need a regex hack (topic_regex) or a code change.
//
// position 0 is the event-name topic, so position/equals alone can also pin
// an exact event name — what event_emitted's event_name does with less
// ceremony. event is optional and, when set, further restricts the rule to
// events whose first topic is that name, exactly like event_emitted's
// event_name but combined with the position check.
type topicPositionParams struct {
	Position int    `json:"position"`
	Equals   string `json:"equals"`
	Event    string `json:"event"`
}

// TopicPosition matches when the decoded topic at the configured position
// equals the configured value.
//
// The comparison is exact string equality against the topic's decoded string
// form (see topicPositionString): string topics compare as themselves,
// numeric topics by their decimal rendering, addresses by their strkey, and
// the single-key RPC wrapper shapes by the value they carry. There is no
// substring, prefix or case-insensitive matching — topic_regex covers the
// pattern questions, and mixing the two here would make a stored rule's
// behaviour hard to predict.
type TopicPosition struct{}

func (TopicPosition) Validate(params json.RawMessage) error {
	p, err := parseTopicPosition(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	if p.Position < 0 {
		details = append(details, FieldError{
			Field:  "position",
			Reason: fmt.Sprintf("topic_position: position %d is negative", p.Position),
		})
	}
	if p.Equals == "" {
		details = append(details, FieldError{
			Field:  "equals",
			Reason: "topic_position: equals is required",
		})
	}
	// No check on position against a maximum: the event's topic count is not
	// known at create time, and a position beyond it is simply a rule that
	// matches nothing (evaluation reports a non-match, never an error).
	if len(details) > 0 {
		return details
	}
	return nil
}

func (TopicPosition) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseTopicPosition(params)
	if err != nil {
		return false, err
	}

	// The optional event filter is the same first-topic check event_emitted
	// makes, so a rule scoped to "deposit" ignores identically-shaped events
	// with a different name.
	if p.Event != "" && ev.EventName() != p.Event {
		return false, nil
	}

	// A position beyond the event's topic count means no match, not an
	// error: the event simply does not carry that topic. A negative position
	// cannot pass Validate, but params could change under a stored rule, so
	// evaluation guards rather than indexing.
	if p.Position < 0 || p.Position >= len(ev.Topics) {
		return false, nil
	}
	return topicPositionString(ev.Topics[p.Position]) == p.Equals, nil
}

// topicPositionString renders a decoded topic as the string the rule compares
// against. Topics arrive in two families: the local XDR decode path emits
// plain Go values (strings, *big.Int for every integer width), while the
// RPC's xdrFormat:"json" path arrives as single-key wrapper objects that
// decodeJSONVal usually normalises — but a rule evaluator must tolerate the
// raw shapes too, which is what the wrapper cases here cover. Anything
// without a string or numeric form (vectors, maps of several keys) renders
// via Canon, which JSON-encodes the value, so a comparison still behaves
// deterministically — those shapes simply never equal a plain configured
// string.
func topicPositionString(topic any) string {
	switch v := topic.(type) {
	case string:
		return v
	case *big.Int:
		return v.String()
	case map[string]any:
		// Wrapper shapes from the RPC path: {"symbol": ...}, {"string": ...},
		// {"address": ...} carry their value as a string; {"i128": ...} and
		// friends carry it as a decimal string, which is the numeric topic's
		// decoded string form. One key means no ambiguity about which value
		// the topic carries.
		for _, key := range []string{"symbol", "string", "address", "i128", "u128", "i64", "u64", "i32", "u32", "u256", "i256"} {
			if s, ok := v[key].(string); ok {
				return s
			}
		}
	}
	return stellar.Canon(topic)
}

// EventNames reports the event name this rule is scoped to, or ok=false when
// no event filter is configured, since such a rule may match events of any
// name and the poller must not narrow a server-side filter on it.
func (TopicPosition) EventNames(params json.RawMessage) ([]string, bool) {
	p, err := parseTopicPosition(params)
	if err != nil || p.Event == "" {
		return nil, false
	}
	return []string{p.Event}, true
}

func (TopicPosition) ParamSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "position", Type: "number", Required: true, Description: "Topic position to compare (0 is the event name)"},
		{Name: "equals", Type: "string", Required: true, Description: "Exact value the decoded topic must equal"},
		{Name: "event", Type: "string", Description: "Only consider events with this name (first topic)"},
	}
}

func parseTopicPosition(params json.RawMessage) (topicPositionParams, error) {
	var p topicPositionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("topic_position: invalid params: %w", err)
	}
	return p, nil
}
