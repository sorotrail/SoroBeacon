package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

func sep41Event(name, addr1, addr2 string, value any) *stellar.DecodedEvent {
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000000",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics: []any{
			map[string]any{"symbol": name},
			map[string]any{"address": addr1},
			map[string]any{"address": addr2},
		},
		Value: value,
	}
}

const (
	alice = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDW"
	bob   = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDX"
	carol = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDY"
)

func TestTokenEventEvaluate(t *testing.T) {
	transfer := sep41Event("transfer", alice, bob, map[string]any{"i128": "1000000"})
	burn := sep41Event("burn", alice, bob, map[string]any{"i128": "500"})
	setAdmin := sep41Event("set_admin", alice, bob, nil)

	tests := []struct {
		name   string
		ev     *stellar.DecodedEvent
		params string
		want   bool
	}{
		{"transfer by name", transfer, `{"event":"transfer"}`, true},
		{"mint does not match transfer rule", transfer, `{"event":"mint"}`, false},
		{"wildcard matches transfer", transfer, `{"event":"*"}`, true},
		{"wildcard matches burn", burn, `{"event":"*"}`, true},
		{"wildcard matches set_admin", setAdmin, `{"event":"*"}`, true},
		{"from filter", transfer, `{"event":"transfer","from":"` + alice + `"}`, true},
		{"from filter mismatch", transfer, `{"event":"transfer","from":"` + carol + `"}`, false},
		{"to filter", transfer, `{"event":"transfer","to":"` + bob + `"}`, true},
		{"burn from is the holder (topic 2)", burn, `{"event":"burn","from":"` + bob + `"}`, true},
		{"burn from is not the admin (topic 1)", burn, `{"event":"burn","from":"` + alice + `"}`, false},
		{"min amount inclusive", transfer, `{"event":"transfer","min_amount":"1000000"}`, true},
		{"min amount above", transfer, `{"event":"transfer","min_amount":"1000001"}`, false},
		{"max amount inclusive", transfer, `{"event":"transfer","max_amount":"1000000"}`, true},
		{"max amount below", transfer, `{"event":"transfer","max_amount":"999999"}`, false},
		{"amount range", transfer, `{"event":"transfer","min_amount":"1","max_amount":"2000000"}`, true},
		{"i128 wider than 64 bits", sep41Event("transfer", alice, bob, map[string]any{"i128": "170141183460469231731687303715884105727"}),
			`{"event":"transfer","min_amount":"170141183460469231731687303715884105726"}`, true},
		{"non-SEP41 event with same name shape", &stellar.DecodedEvent{Topics: []any{"transfer"}}, `{"event":"transfer"}`, false},
		{"from and to together", transfer, `{"event":"transfer","from":"` + alice + `","to":"` + bob + `"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (TokenEvent{}).Evaluate(context.Background(), tt.ev, json.RawMessage(tt.params))
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTokenEventValidate(t *testing.T) {
	ok := []string{
		`{"event":"transfer"}`,
		`{"event":"*"}`,
		`{"event":"burn","from":"GA...","min_amount":"10"}`,
		`{"event":"set_admin"}`,
	}
	for _, p := range ok {
		if err := (TokenEvent{}).Validate(json.RawMessage(p)); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", p, err)
		}
	}
	bad := map[string]string{
		`{}`:                                     "event is required",
		`{"event":"freeze"}`:                     "unknown event",
		`{"event":"transfer","min_amount":"x"}`:  "not a decimal integer",
		`{"event":"set_admin","min_amount":"1"}`: "carries no amount",
	}
	for p, want := range bad {
		err := (TokenEvent{}).Validate(json.RawMessage(p))
		if err == nil {
			t.Errorf("Validate(%s) = nil, want error containing %q", p, want)
			continue
		}
		if !contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %q, want error containing %q", p, err.Error(), want)
		}
	}
}

func TestTokenEventRegistered(t *testing.T) {
	r := NewRegistry()
	if err := r.Validate(TypeTokenEvent, json.RawMessage(`{"event":"transfer"}`)); err != nil {
		t.Fatalf("token_event not registered: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
