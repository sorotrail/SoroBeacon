package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func TestSelfTransferEvaluate(t *testing.T) {
	bare := func(name, from, to string, value any) *stellar.DecodedEvent {
		return &stellar.DecodedEvent{
			Topics: []any{name, from, to},
			Value:  value,
		}
	}
	amount := map[string]any{"i128": "1000000"}

	tests := []struct {
		name   string
		ev     *stellar.DecodedEvent
		params string
		want   bool
	}{
		{"self transfer wrapped topics", sep41Event("transfer", alice, alice, amount), `{}`, true},
		{"self transfer bare topics", bare("transfer", alice, alice, amount), `{}`, true},
		{"normal transfer does not match", sep41Event("transfer", alice, bob, amount), `{}`, false},
		{"non-transfer event does not match", sep41Event("set_admin", alice, alice, nil), `{}`, false},
		{"no address slots does not match", &stellar.DecodedEvent{Topics: []any{"transfer"}}, `{}`, false},
		{"empty value without min matches", sep41Event("transfer", alice, alice, nil), `{}`, true},
		{"min_amount inclusive", sep41Event("transfer", alice, alice, amount), `{"min_amount":"1000000"}`, true},
		{"min_amount above is a miss", sep41Event("transfer", alice, alice, amount), `{"min_amount":"1000001"}`, false},
		{"min_amount with no amount is a miss", sep41Event("transfer", alice, alice, nil), `{"min_amount":"1"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (SelfTransfer{}).Evaluate(context.Background(), tt.ev, json.RawMessage(tt.params))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSelfTransferValidate(t *testing.T) {
	ok := []string{`{}`, `{"min_amount":"0"}`, `{"min_amount":"170141183460469231731687303715884105727"}`}
	for _, p := range ok {
		assert.NoError(t, (SelfTransfer{}).Validate(json.RawMessage(p)), "params %s", p)
	}
	assert.Error(t, (SelfTransfer{}).Validate(json.RawMessage(`{"min_amount":"x"}`)))
	assert.Error(t, (SelfTransfer{}).Validate(json.RawMessage(`{"min_amount":1.5}`)))
}

func TestSelfTransferRegistered(t *testing.T) {
	if err := NewRegistry().Validate(TypeSelfTransfer, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("self_transfer not registered: %v", err)
	}
}
