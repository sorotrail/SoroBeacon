package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// SEP-41 is Stellar's token contract interface. A token implementing it
// emits these events, with topics at fixed positions:
//
//	transfer  {sym:"transfer", addr:from, addr:to}        value: i128 amount
//	mint      {sym:"mint",     addr:admin, addr:to}        value: i128 amount
//	burn      {sym:"burn",     addr:admin, addr:from}      value: i128 amount
//	clawback  {sym:"clawback", addr:admin, addr:from}      value: i128 amount
//	set_admin {sym:"set_admin", addr:admin, addr:new}      value: void
//
// The token_event rule type matches these by name and, for the amount
// events, against an optional amount threshold — the two questions people
// actually ask of a token: "did X move?" and "did a big one move?".
//
// token_event exists alongside the generic event_emitted rather than
// instead of it: event_emitted stays fully general (any contract, any
// shape), while token_event knows SEP-41's topic positions, so params can
// say from/to without the user hand-counting topic indices.
//
// Params:
//
//	{
//	  "event": "transfer",        // required: transfer|mint|burn|clawback|set_admin|*
//	  "from": "GA...",            // optional: exact address in the from slot
//	  "to":   "GA...",            // optional: exact address in the to slot
//	  "min_amount": "1000000",    // optional: i128 amount >= this (decimal string)
//	  "max_amount": "1000000"     // optional: i128 amount <= this
//	}
//
// event "*" matches any of the amount-bearing SEP-41 events.
type tokenEventParams struct {
	Event     string `json:"event"`
	From      string `json:"from"`
	To        string `json:"to"`
	MinAmount string `json:"min_amount"`
	MaxAmount string `json:"max_amount"`
}

// sep41Events maps the SEP-41 event names to their topic layout.
// Index 0 is the event name; 1 and 2 are the two address slots. The
// direction of each slot (admin vs recipient, admin vs holder) is
// event-specific; from/to in params abstract over it:
//
//	transfer: from = topic 1, to = topic 2
//	mint:     from = topic 1 (admin), to = topic 2 (recipient)
//	burn:     from = topic 2 (holder), to = topic 1 (admin)
//	clawback: from = topic 2 (holder), to = topic 1 (admin)
//	set_admin: from = topic 1 (old admin), to = topic 2 (new admin)
var sep41Events = map[string][2]int{
	"transfer":  {1, 2},
	"mint":      {1, 2},
	"burn":      {2, 1},
	"clawback":  {2, 1},
	"set_admin": {1, 2},
}

// TokenEvent matches SEP-41 token events by name and optional
// from/to/amount constraints.
type TokenEvent struct{}

const (
	TypeTokenEvent = "token_event"
)

func (TokenEvent) Validate(params json.RawMessage) error {
	p, err := parseTokenEvent(params)
	if err != nil {
		return err
	}
	if p.Event == "" {
		return fmt.Errorf("token_event: event is required (transfer|mint|burn|clawback|set_admin|*)")
	}
	if p.Event != "*" {
		if _, ok := sep41Events[p.Event]; !ok {
			return fmt.Errorf("token_event: unknown event %q (want transfer|mint|burn|clawback|set_admin|*)", p.Event)
		}
	}
	if _, ok := new(big.Int).SetString(p.MinAmount, 10); p.MinAmount != "" && !ok {
		return fmt.Errorf("token_event: min_amount %q is not a decimal integer", p.MinAmount)
	}
	if _, ok := new(big.Int).SetString(p.MaxAmount, 10); p.MaxAmount != "" && !ok {
		return fmt.Errorf("token_event: max_amount %q is not a decimal integer", p.MaxAmount)
	}
	// set_admin carries no value; amount filters on it can never match.
	if p.Event == "set_admin" && (p.MinAmount != "" || p.MaxAmount != "") {
		return fmt.Errorf("token_event: set_admin carries no amount; remove min_amount/max_amount")
	}
	return nil
}

func (TokenEvent) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseTokenEvent(params)
	if err != nil {
		return false, err
	}

	// The event name is the first topic as a symbol; a non-SEP-41 event
	// simply does not match.
	if len(ev.Topics) < 3 {
		return false, nil
	}
	name := ev.EventName()
	slots, isSep41 := sep41Events[name]
	if p.Event != "*" {
		if !isSep41 || name != p.Event {
			return false, nil
		}
	} else if !isSep41 {
		return false, nil
	}

	// Address slots: from = the outgoing/admin side, to = the incoming
	// side, per the table above.
	if p.From != "" && addr(ev.Topics[slots[0]]) != p.From {
		return false, nil
	}
	if p.To != "" && addr(ev.Topics[slots[1]]) != p.To {
		return false, nil
	}

	// Amount: the event value as an i128. "*" matches any amount-bearing
	// SEP-41 event, so events without a value fail only if a threshold was
	// actually requested.
	if p.MinAmount == "" && p.MaxAmount == "" {
		return true, nil
	}
	amount, ok := amount(ev.Value)
	if !ok {
		return false, nil
	}
	if p.MinAmount != "" && amount.Cmp(parseBig(p.MinAmount)) < 0 {
		return false, nil
	}
	if p.MaxAmount != "" && amount.Cmp(parseBig(p.MaxAmount)) > 0 {
		return false, nil
	}
	return true, nil
}

// addr extracts the address string from a decoded topic value, which may be
// the single-key wrapper ({"address": "G..."}) or a bare string.
func addr(topic any) string {
	switch v := topic.(type) {
	case map[string]any:
		if s, ok := v["address"].(string); ok {
			return s
		}
	case string:
		return v
	}
	return ""
}

// amount extracts the i128 amount from a decoded event value. Values arrive
// as the single-key wrapper {"i128": "..."} with the integer as a decimal
// string — wide integers must not pass through float64, so this is big.Int
// end to end.
func amount(value any) (*big.Int, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	s, ok := m["i128"].(string)
	if !ok {
		// Some decoders may emit a JSON number for small i128s.
		if f, ok := m["i128"].(float64); ok {
			return big.NewInt(int64(f)), true
		}
		return nil, false
	}
	n, ok := new(big.Int).SetString(s, 10)
	return n, ok
}

func parseBig(s string) *big.Int {
	n, _ := new(big.Int).SetString(s, 10)
	return n
}

func parseTokenEvent(params json.RawMessage) (tokenEventParams, error) {
	var p tokenEventParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("token_event: invalid params: %w", err)
	}
	return p, nil
}
