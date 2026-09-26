package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// tokenSupplyChangeParams configure the token_supply_change rule.
//
//	{
//	  "direction": "any",         // optional: mint|burn|any (default any)
//	  "min_amount": "1000000"     // optional: i128 amount >= this (decimal string)
//	}
//
// Mints and burns change a token's total supply — the event class that
// matters most to anyone holding or pricing that token, since an unexpected
// mint is the classic rug signal. token_event can match mint or burn
// individually, but cannot express "any supply change above N" in one rule;
// this rule does, and the direction stays available for the alert payload in
// the matched event's name.
//
// clawback is deliberately not treated as a supply change: it moves tokens
// out of a holder's balance back to the issuer, it does not create or
// destroy supply, so a supply-monitoring rule that fired on clawbacks would
// cry wolf on an administrative action.
type tokenSupplyChangeParams struct {
	Direction string `json:"direction"`
	MinAmount string `json:"min_amount"`
}

// TokenSupplyChange matches SEP-41 mint and burn events, optionally filtered
// by direction and by a minimum i128 amount.
type TokenSupplyChange struct{}

func (TokenSupplyChange) Validate(params json.RawMessage) error {
	p, err := parseTokenSupplyChange(params)
	if err != nil {
		return err
	}
	var details FieldErrors
	switch p.Direction {
	case "", "mint", "burn", "any":
	default:
		details = append(details, FieldError{
			Field:  "direction",
			Reason: fmt.Sprintf("token_supply_change: unknown direction %q (want mint|burn|any)", p.Direction),
		})
	}
	if _, ok := new(big.Int).SetString(p.MinAmount, 10); p.MinAmount != "" && !ok {
		details = append(details, FieldError{
			Field:  "min_amount",
			Reason: fmt.Sprintf("token_supply_change: min_amount %q is not a decimal integer", p.MinAmount),
		})
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

func (TokenSupplyChange) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseTokenSupplyChange(params)
	if err != nil {
		return false, err
	}
	switch p.Direction {
	case "", "mint", "burn", "any":
	default:
		return false, fmt.Errorf("token_supply_change: invalid params: unknown direction %q", p.Direction)
	}
	var minAmount *big.Int
	if p.MinAmount != "" {
		var ok bool
		minAmount, ok = new(big.Int).SetString(p.MinAmount, 10)
		if !ok {
			return false, fmt.Errorf("token_supply_change: invalid params: min_amount %q is not a decimal integer", p.MinAmount)
		}
	}

	// EventName handles both decoded topic shapes (a bare string on the XDR
	// decode path and the {"symbol": "..."} wrapper on the RPC
	// xdrFormat:"json" path), so only mint and burn pass regardless of which
	// path decoded the event. Anything else — transfers, clawbacks,
	// set_admin, non-SEP-41 events — simply does not match.
	name := ev.EventName()
	if name != "mint" && name != "burn" {
		return false, nil
	}
	if p.Direction == "mint" || p.Direction == "burn" {
		if name != p.Direction {
			return false, nil
		}
	}

	if minAmount == nil {
		return true, nil
	}
	// The amount decodes via token_event's i128 helper, which keeps wide
	// integers in big.Int end to end rather than passing through float64.
	// An event without a decodable amount cannot clear a threshold, so it is
	// a non-match rather than an error.
	value, ok := supplyAmount(ev.Value)
	if !ok {
		return false, nil
	}
	return value.Cmp(minAmount) >= 0, nil
}

// supplyAmount extracts the i128 amount from a decoded mint/burn value. The
// map wrapper shape ({"i128": "..."}) is handled by token_event's amount
// helper; the decoder itself emits bare *big.Int for every integer type, so
// that shape is handled here first.
func supplyAmount(value any) (*big.Int, bool) {
	switch t := value.(type) {
	case *big.Int:
		return t, true
	case big.Int:
		c := new(big.Int).Set(&t)
		return c, true
	default:
		return amount(value)
	}
}

// EventNames reports the SEP-41 supply events this rule is scoped to. An
// unrestricted rule ("any" or unset) matches both mint and burn and reports
// them both; a direction-restricted rule reports that one name, so the
// poller can narrow its server-side getEvents filter.
func (TokenSupplyChange) EventNames(params json.RawMessage) ([]string, bool) {
	p, err := parseTokenSupplyChange(params)
	if err != nil {
		return nil, false
	}
	switch p.Direction {
	case "mint", "burn":
		return []string{p.Direction}, true
	case "", "any":
		return []string{"mint", "burn"}, true
	default:
		return nil, false
	}
}

func parseTokenSupplyChange(params json.RawMessage) (tokenSupplyChangeParams, error) {
	var p tokenSupplyChangeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("token_supply_change: invalid params: %w", err)
	}
	return p, nil
}
