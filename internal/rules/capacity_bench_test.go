package rules

import (
	"context"
	"encoding/json"
	"math/big"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// ruleCapacityCase pairs one registered rule type with realistic params and a
// matching and non-matching decoded event so every rule type in NewRegistry can
// be measured on identical hardware and asserted for completeness in CI.
type ruleCapacityCase struct {
	ruleType string
	params   json.RawMessage
	matchEv  *stellar.DecodedEvent
	missEv   *stellar.DecodedEvent
}

func ruleCapacityCases() []ruleCapacityCase {
	const (
		contractAllow = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"
		contractOther = "CBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM"
		addrAlice     = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
		addrBob       = "GBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBC7S"
	)

	wed10UTC := time.Date(2026, time.September, 23, 10, 0, 0, 0, time.UTC)
	wed20UTC := time.Date(2026, time.September, 23, 20, 0, 0, 0, time.UTC)

	stdTransfer := func(amount int64, contract, from, to string, closedAt time.Time) *stellar.DecodedEvent {
		return &stellar.DecodedEvent{
			ID:             "0000000012884905986-0000000001",
			ContractID:     contract,
			Ledger:         1000,
			LedgerClosedAt: closedAt,
			Topics:         []any{"transfer", from, to},
			Value:          map[string]any{"amount": big.NewInt(amount), "memo": "bench"},
		}
	}

	mintEv := &stellar.DecodedEvent{
		ID:             "0000000012884905986-0000000002",
		ContractID:     contractAllow,
		Ledger:         1001,
		LedgerClosedAt: wed10UTC,
		Topics:         []any{"mint", addrAlice, addrBob},
		Value:          big.NewInt(5_000_000),
	}

	return []ruleCapacityCase{
		{
			ruleType: TypeEventEmitted,
			params:   json.RawMessage(`{"event_name":"transfer"}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   mintEv,
		},
		{
			ruleType: TypeValueThreshold,
			params:   json.RawMessage(`{"event_name":"transfer","value_path":"amount","comparison":"gt","threshold":100}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   stdTransfer(50, contractAllow, addrAlice, addrBob, wed10UTC),
		},
		{
			ruleType: TypeContractAllowlist,
			params:   json.RawMessage(`{"contract_ids":["` + contractAllow + `"]}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   stdTransfer(500, contractOther, addrAlice, addrBob, wed10UTC),
		},
		{
			ruleType: TypeEventNameGlob,
			params:   json.RawMessage(`{"patterns":["swap_*","trans*"]}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   mintEv,
		},
		{
			ruleType: TypeNumericRange,
			params:   json.RawMessage(`{"event_name":"transfer","value_path":"amount","min":"100","max":"1000"}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   stdTransfer(5000, contractAllow, addrAlice, addrBob, wed10UTC),
		},
		{
			ruleType: TypeTokenSupplyChange,
			params:   json.RawMessage(`{"direction":"mint","min_amount":"1000000"}`),
			matchEv:  mintEv,
			missEv:   stdTransfer(5_000_000, contractAllow, addrAlice, addrBob, wed10UTC),
		},
		{
			ruleType: TypeTokenEvent,
			params:   json.RawMessage(`{"event":"transfer","from":"` + addrAlice + `","min_amount":"100"}`),
			matchEv:  sep41Event("transfer", addrAlice, addrBob, map[string]any{"i128": "500"}),
			missEv:   sep41Event("transfer", addrBob, addrAlice, map[string]any{"i128": "500"}),
		},
		{
			ruleType: TypeSelfTransfer,
			params:   json.RawMessage(`{"min_amount":"100"}`),
			matchEv:  sep41Event("transfer", addrAlice, addrAlice, map[string]any{"i128": "500"}),
			missEv:   sep41Event("transfer", addrAlice, addrBob, map[string]any{"i128": "500"}),
		},
		{
			ruleType: TypeTimeWindow,
			params:   json.RawMessage(`{"start":"09:00","end":"17:00","days":["wed"]}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   stdTransfer(500, contractAllow, addrAlice, addrBob, wed20UTC),
		},
		{
			ruleType: TypeFrequencyThreshold,
			params:   json.RawMessage(`{"event_name":"transfer","count":1,"window":"5m"}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   mintEv,
		},
		{
			ruleType: TypeComposite,
			params:   json.RawMessage(`{"op":"and","rules":[{"type":"event_emitted","params":{"event_name":"transfer"}},{"type":"value_threshold","params":{"value_path":"amount","comparison":"gt","threshold":100}}]}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   stdTransfer(50, contractAllow, addrAlice, addrBob, wed10UTC),
		},
		{
			ruleType: TypeTopicRegex,
			params:   json.RawMessage(`{"position":0,"pattern":"^trans(fer|act)$"}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   mintEv,
		},
		{
			ruleType: TypeAddressWatchlist,
			params:   json.RawMessage(`{"addresses":["` + addrAlice + `"],"match":"from","event":"transfer"}`),
			matchEv:  &stellar.DecodedEvent{ID: "ev-watch-1", ContractID: contractAllow, Topics: []any{"transfer", addrAlice, addrBob}, Value: big.NewInt(500)},
			missEv:   &stellar.DecodedEvent{ID: "ev-watch-2", ContractID: contractAllow, Topics: []any{"transfer", addrBob, addrBob}, Value: big.NewInt(500)},
		},
		{
			ruleType: TypeTopicPosition,
			params:   json.RawMessage(`{"position":1,"equals":"` + addrAlice + `","event":"transfer"}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   stdTransfer(500, contractAllow, addrBob, addrAlice, wed10UTC),
		},
		{
			ruleType: TypeBaselineDeviation,
			params:   json.RawMessage(`{"event_name":"transfer","value_path":"amount","window":"24h","warmup":"0s","min_samples":3,"sensitivity":3}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   stdTransfer(100, contractAllow, addrAlice, addrBob, wed10UTC),
		},
		{
			ruleType: TypeAbsenceOfEvent,
			params:   json.RawMessage(`{"event_name":"transfer","window":"30m"}`),
			matchEv:  stdTransfer(500, contractAllow, addrAlice, addrBob, wed10UTC),
			missEv:   mintEv,
		},
	}
}

// TestCapacityCasesCoverEveryRegisteredRuleType ensures the capacity model's
// per-rule benchmark table never drifts behind NewRegistry(). Adding a new
// rule type without a benchmark fixture fails `go test ./...`.
func TestCapacityCasesCoverEveryRegisteredRuleType(t *testing.T) {
	reg := NewRegistry()
	cases := ruleCapacityCases()

	covered := make(map[string]ruleCapacityCase, len(cases))
	for _, c := range cases {
		covered[c.ruleType] = c
	}

	registered := reg.Types()
	sort.Strings(registered)
	for _, rt := range registered {
		c, ok := covered[rt]
		require.Truef(t, ok, "registered rule type %q is missing from ruleCapacityCases()", rt)
		require.NoErrorf(t, reg.Validate(rt, c.params), "rule type %q fixture failed Validate", rt)

		if abs, isAbs := reg.Absence(rt); isAbs {
			spec, err := abs.Spec(c.params)
			require.NoError(t, err)
			assert.Truef(t, abs.Matches(c.matchEv, spec), "absence type %q must match matchEv", rt)
			assert.Falsef(t, abs.Matches(c.missEv, spec), "absence type %q must not match missEv", rt)
			continue
		}

		ctx := WithRuleID(context.Background(), 1)
		if rt == TypeBaselineDeviation {
			for range 3 {
				_, err := reg.Evaluate(ctx, rt, c.missEv, c.params)
				require.NoError(t, err)
			}
		}
		matched, err := reg.Evaluate(ctx, rt, c.matchEv, c.params)
		require.NoErrorf(t, err, "rule type %q Evaluate(matchEv)", rt)
		assert.Truef(t, matched, "rule type %q expected match=true on matchEv", rt)

		matched, err = reg.Evaluate(ctx, rt, c.missEv, c.params)
		require.NoErrorf(t, err, "rule type %q Evaluate(missEv)", rt)
		assert.Falsef(t, matched, "rule type %q expected match=false on missEv", rt)
	}
}

// BenchmarkRuleTypeCapacity measures evaluation cost separately for every rule
// type registered in NewRegistry, across both matching and non-matching events.
func BenchmarkRuleTypeCapacity(b *testing.B) {
	reg := NewRegistry()
	// Advance the frequency rule's clock past its 5m window on each call so
	// BenchmarkRuleTypeCapacity/frequency_threshold/match measures steady-state
	// window pruning and crossing rather than accumulating an unbounded slice
	// inside a single 5-minute episode.
	clockTick := time.Date(2026, time.September, 23, 10, 0, 0, 0, time.UTC)
	reg.Register(TypeFrequencyThreshold, NewFrequencyThreshold().WithClock(func() time.Time {
		clockTick = clockTick.Add(10 * time.Minute)
		return clockTick
	}))
	ctx := WithRuleID(context.Background(), 42)

	for _, tc := range ruleCapacityCases() {
		tc := tc
		b.Run(tc.ruleType, func(b *testing.B) {
			if abs, isAbs := reg.Absence(tc.ruleType); isAbs {
				b.Run("match", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						spec, err := abs.Spec(tc.params)
						if err != nil {
							b.Fatal(err)
						}
						if !abs.Matches(tc.matchEv, spec) {
							b.Fatal("expected match")
						}
					}
				})
				b.Run("nomatch", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						spec, err := abs.Spec(tc.params)
						if err != nil {
							b.Fatal(err)
						}
						if abs.Matches(tc.missEv, spec) {
							b.Fatal("expected nomatch")
						}
					}
				})
				return
			}

			b.Run("match", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, err := reg.Evaluate(ctx, tc.ruleType, tc.matchEv, tc.params)
					if err != nil {
						b.Fatal(err)
					}
					if !got {
						b.Fatal("expected match")
					}
				}
			})
			b.Run("nomatch", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, err := reg.Evaluate(ctx, tc.ruleType, tc.missEv, tc.params)
					if err != nil {
						b.Fatal(err)
					}
					if got {
						b.Fatal("expected nomatch")
					}
				}
			})
		})
	}
}
