package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// self_transfer matches SEP-41 `transfer` events whose from and to slots
// hold the same address. A self-transfer is almost always a contract bug or
// deliberate wash activity used to fake volume, and neither is expressible
// as a plain token_event without one rule per address pair.
//
// It is deliberately limited to `transfer`: a self "mint" or "burn" is
// admin/holder bookkeeping rather than a transfer, and events such as
// `set_admin` have admin slots, not from/to slots, so they never match.
// Events that are not transfers (or that carry no address slots) are
// non-matches, not errors.
//
// Params:
//
//	{
//	  "min_amount": "1000000"   // optional: i128 amount >= this (decimal string)
//	}
type selfTransferParams struct {
	MinAmount string `json:"min_amount"`
}

// TypeSelfTransfer is the registry name of the self_transfer rule.
const TypeSelfTransfer = "self_transfer"

// SelfTransfer matches SEP-41 transfers sent from an address to itself.
type SelfTransfer struct{}

func (SelfTransfer) Validate(params json.RawMessage) error {
	p, err := parseSelfTransfer(params)
	if err != nil {
		return err
	}
	if p.MinAmount == "" {
		return nil
	}
	if _, ok := new(big.Int).SetString(p.MinAmount, 10); !ok {
		return FieldErrors{{
			Field:  "min_amount",
			Reason: fmt.Sprintf("self_transfer: min_amount %q is not a decimal integer", p.MinAmount),
		}}
	}
	return nil
}

func (SelfTransfer) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseSelfTransfer(params)
	if err != nil {
		return false, err
	}

	// Only a transfer has from/to slots in the sense this rule means. A
	// non-transfer event, or one missing its address topics, does not match.
	if ev.EventName() != "transfer" || len(ev.Topics) < 3 {
		return false, nil
	}
	from := addr(ev.Topics[1])
	to := addr(ev.Topics[2])
	if from == "" || to == "" || from != to {
		return false, nil
	}

	if p.MinAmount == "" {
		return true, nil
	}
	amt, ok := amount(ev.Value)
	if !ok {
		return false, nil
	}
	return amt.Cmp(parseBig(p.MinAmount)) >= 0, nil
}

func parseSelfTransfer(params json.RawMessage) (selfTransferParams, error) {
	var p selfTransferParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("self_transfer: invalid params: %w", err)
	}
	return p, nil
}
