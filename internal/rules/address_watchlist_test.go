package rules

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/stellar/go-stellar-sdk/strkey"
)

// mustAddress derives a distinct valid account strkey from a seed, so tests
// can build hundreds of real addresses without hand-writing them. Encode
// cannot fail for a 32-byte payload, so the error is a panic rather than a
// test failure at every call site.
func mustAddress(seed int) string {
	raw := make([]byte, 32)
	raw[0] = byte(seed)
	raw[1] = byte(seed >> 8)
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw)
	if err != nil {
		panic(err)
	}
	return s
}

func watchlistAddress(tb testing.TB, seed int) string {
	tb.Helper()
	return mustAddress(seed)
}

// Unlike token_event's fixture addresses, watchlist addresses must be real
// strkeys — the rule validates them at create time.
var (
	wlAlice = mustAddress(0x0A1)
	wlBob   = mustAddress(0x0B2)
	wlCarol = mustAddress(0x0C3)
	// Off-list addresses for miss cases: seeds above the ranges the other
	// fixtures and the large-watchlist test use, so they collide with none.
	wlDave = mustAddress(0x2EE)
	wlEve  = mustAddress(0x2EF)
)

// watchlistEvent builds a SEP-41 event with arbitrary topic values, so tests
// can cover both decoded topic shapes: the RPC's xdrFormat:"json" path emits
// single-key wrappers ({"symbol": ...}, {"address": ...}) while the local
// XDR decode path emits bare strings.
func watchlistEvent(name string, symbol bool, slot1, slot2 any) *stellar.DecodedEvent {
	var topic0 any = name
	if symbol {
		topic0 = map[string]any{"symbol": name}
	}
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000000",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics:     []any{topic0, slot1, slot2},
		Value:      map[string]any{"i128": "1000000"},
	}
}

func TestAddressWatchlistEvaluate(t *testing.T) {
	transfer := func(slot1, slot2 any) *stellar.DecodedEvent {
		return watchlistEvent("transfer", false, slot1, slot2)
	}
	burn := func(slot1, slot2 any) *stellar.DecodedEvent {
		return watchlistEvent("burn", false, slot1, slot2)
	}

	tests := []struct {
		name   string
		ev     *stellar.DecodedEvent
		params string
		want   bool
	}{
		// from/either/to matching on a transfer (from = topic 1, to = topic 2).
		{"from match", transfer(wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"match":"from"}`, true},
		{"from mismatch", transfer(wlBob, wlAlice), `{"addresses":["` + wlAlice + `"],"match":"from"}`, false},
		{"to match", transfer(wlAlice, wlBob), `{"addresses":["` + wlBob + `"],"match":"to"}`, true},
		{"to mismatch", transfer(wlBob, wlAlice), `{"addresses":["` + wlBob + `"],"match":"to"}`, false},
		{"either matches from", transfer(wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"match":"either"}`, true},
		{"either matches to", transfer(wlAlice, wlBob), `{"addresses":["` + wlBob + `"],"match":"either"}`, true},
		{"either matches only one slot", transfer(wlBob, wlCarol), `{"addresses":["` + wlCarol + `"],"match":"either"}`, true},
		{"no match", transfer(wlBob, wlCarol), `{"addresses":["` + wlAlice + `"]}`, false},
		{"both slots on the list", transfer(wlAlice, wlAlice), `{"addresses":["` + wlAlice + `"]}`, true},
		{"default match is either", transfer(wlBob, wlCarol), `{"addresses":["` + wlBob + `"]}`, true},
		{"multiple addresses", transfer(wlBob, wlCarol), `{"addresses":["` + wlAlice + `","` + wlCarol + `"]}`, true},

		// Matching is exact and case-sensitive: no partial, prefix or
		// case-folded matching. Strkeys are uppercase base32, so a lowercase
		// spelling is a different string that cannot match.
		{"prefix of a watched address does not match", transfer(wlAlice+"XXXX", wlBob), `{"addresses":["` + wlAlice + `"]}`, false},
		{"lowercased address does not match", transfer(strings.ToLower(wlAlice), wlBob), `{"addresses":["` + wlAlice + `"]}`, false},

		// Semantic slots: on burn the outgoing slot is the holder, not the
		// admin — the same convention token_event pins.
		{"burn from is the holder (topic 2)", burn(wlBob, wlAlice), `{"addresses":["` + wlAlice + `"],"match":"from"}`, true},
		{"burn from is not the admin (topic 1)", burn(wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"match":"from"}`, false},
		{"burn holder via either", burn(wlAlice, wlBob), `{"addresses":["` + wlBob + `"]}`, true},
		{"burn to is the admin (topic 1)", burn(wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"match":"to"}`, true},

		// Event-name restriction.
		{"event restriction hit", transfer(wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"event":"transfer"}`, true},
		{"event restriction miss", transfer(wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"event":"mint"}`, false},
		{"wildcard event", transfer(wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"event":"*"}`, true},
		{"restriction keeps slot semantics on burn", burn(wlAlice, wlBob), `{"addresses":["` + wlBob + `"],"event":"burn"}`, true},

		// Both decoded topic shapes: bare strings (XDR decode path) and the
		// {"symbol": ...} / {"address": ...} wrappers (RPC xdrFormat:"json").
		{"wrapped topics match", transfer(
			map[string]any{"address": wlAlice},
			map[string]any{"address": wlBob},
		), `{"addresses":["` + wlAlice + `"]}`, true},
		{"wrapped topics no match", transfer(
			map[string]any{"address": wlBob},
			map[string]any{"address": wlCarol},
		), `{"addresses":["` + wlAlice + `"]}`, false},
		{"symbol-wrapped name", watchlistEvent("transfer", true, wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"event":"transfer"}`, true},
		{"symbol-wrapped name restricted out", watchlistEvent("transfer", true, wlAlice, wlBob), `{"addresses":["` + wlAlice + `"],"event":"mint"}`, false},
		{"mixed shapes", transfer(wlAlice, map[string]any{"address": wlBob}), `{"addresses":["` + wlBob + `"]}`, true},

		// Non-SEP-41 events and malformed topics never match.
		{"non-SEP41 event", watchlistEvent("swap", false, wlAlice, wlBob), `{"addresses":["` + wlAlice + `"]}`, false},
		{"too few topics", &stellar.DecodedEvent{Topics: []any{"transfer"}}, `{"addresses":["` + wlAlice + `"]}`, false},
		{"non-address topic in slot", transfer(map[string]any{"symbol": wlAlice}, wlBob), `{"addresses":["` + wlAlice + `"]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := new(AddressWatchlist).Evaluate(context.Background(), tt.ev, json.RawMessage(tt.params))
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAddressWatchlistValidate(t *testing.T) {
	ok := []string{
		`{"addresses":["` + wlAlice + `"]}`,
		`{"addresses":["` + wlAlice + `","` + wlBob + `"]}`,
		`{"addresses":["` + wlAlice + `"],"match":"from"}`,
		`{"addresses":["` + wlAlice + `"],"match":"to"}`,
		`{"addresses":["` + wlAlice + `"],"match":"either"}`,
		`{"addresses":["` + wlAlice + `"],"event":"transfer"}`,
		`{"addresses":["` + wlAlice + `"],"event":"*"}`,
	}
	for _, p := range ok {
		if err := new(AddressWatchlist).Validate(json.RawMessage(p)); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", p, err)
		}
	}
	bad := map[string]string{
		`{}`:                               "addresses is required",
		`{"addresses":[]}`:                 "addresses is required",
		`{"addresses":["not-an-address"]}`: "not a plausible Stellar address",
		`{"addresses":["GAAA"]}`:           "not a plausible Stellar address",
		`{"addresses":["` + strings.ToLower(wlAlice) + `"]}`: "not a plausible Stellar address",
		`{"addresses":["` + wlAlice + `"],"match":"both"}`:   "unknown match",
		`{"addresses":["` + wlAlice + `"],"match":"FROM"}`:   "unknown match",
		`{"addresses":["` + wlAlice + `"],"event":"freeze"}`: "unknown event",
	}
	for p, want := range bad {
		err := new(AddressWatchlist).Validate(json.RawMessage(p))
		if err == nil {
			t.Errorf("Validate(%s) = nil, want error containing %q", p, want)
			continue
		}
		if !contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %q, want error containing %q", p, err.Error(), want)
		}
	}
}

func TestAddressWatchlistValidateAcceptsContractAddress(t *testing.T) {
	// Token contracts move tokens too; a C... strkey belongs on a watchlist.
	p := `{"addresses":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"]}`
	if err := new(AddressWatchlist).Validate(json.RawMessage(p)); err != nil {
		t.Errorf("Validate(contract address) = %v, want nil", err)
	}
}

func TestAddressWatchlistValidateHasNoEventRequirement(t *testing.T) {
	// The issue's contract: the watchlist validates without needing an event.
	if err := new(AddressWatchlist).Validate(json.RawMessage(`{"addresses":["` + wlAlice + `"]}`)); err != nil {
		t.Fatalf("Validate without event = %v, want nil", err)
	}
}

func TestAddressWatchlistValidateChecksEveryAddress(t *testing.T) {
	// One bad entry among good ones is rejected, and the error names its index.
	p := `{"addresses":["` + wlAlice + `","oops","` + wlBob + `"]}`
	err := new(AddressWatchlist).Validate(json.RawMessage(p))
	if err == nil {
		t.Fatal("Validate with one bad address = nil, want error")
	}
	if !contains(err.Error(), "addresses[1]") {
		t.Errorf("Validate error %q, want it to name addresses[1]", err.Error())
	}
}

// TestAddressWatchlistRegistered pins the registry wiring the dashboard
// dropdown and the API both read.
func TestAddressWatchlistRegistered(t *testing.T) {
	r := NewRegistry()
	if err := r.Validate(TypeAddressWatchlist, json.RawMessage(`{"addresses":["`+wlAlice+`"]}`)); err != nil {
		t.Fatalf("address_watchlist not registered: %v", err)
	}
	got, err := r.Evaluate(context.Background(), TypeAddressWatchlist, watchlistEvent("transfer", false, wlAlice, wlBob), json.RawMessage(`{"addresses":["`+wlAlice+`"]}`))
	if err != nil {
		t.Fatalf("evaluate via registry: %v", err)
	}
	if !got {
		t.Fatal("evaluate via registry = false, want true")
	}
}

// TestAddressWatchlistEventNames pins the EventNamer contract the poller
// uses to narrow its server-side getEvents filter.
func TestAddressWatchlistEventNames(t *testing.T) {
	all, ok := new(AddressWatchlist).EventNames(json.RawMessage(`{"addresses":["` + wlAlice + `"]}`))
	if !ok || len(all) != len(sep41Events) {
		t.Errorf("EventNames(unrestricted) = %v, %v; want all %d SEP-41 events, true", all, ok, len(sep41Events))
	}
	one, ok := new(AddressWatchlist).EventNames(json.RawMessage(`{"addresses":["` + wlAlice + `"],"event":"transfer"}`))
	if !ok || len(one) != 1 || one[0] != "transfer" {
		t.Errorf("EventNames(transfer) = %v, %v; want [transfer], true", one, ok)
	}
}

// TestAddressWatchlistLargeWatchlist pins the set-building requirement: a
// watchlist of hundreds of addresses must work, must not accept an address
// it does not contain, and — because the set is built once and memoised —
// its per-event cost must not grow with the list length.
func TestAddressWatchlistLargeWatchlist(t *testing.T) {
	const n = 500
	addrs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		addrs = append(addrs, watchlistAddress(t, i))
	}
	paramsB, err := json.Marshal(map[string]any{"addresses": addrs})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	params := json.RawMessage(paramsB)
	if err := new(AddressWatchlist).Validate(params); err != nil {
		t.Fatalf("Validate large watchlist: %v", err)
	}
	e := new(AddressWatchlist)
	got, err := e.Evaluate(context.Background(), watchlistEvent("transfer", false, addrs[n-1], wlBob), params)
	if err != nil {
		t.Fatalf("evaluate hit: %v", err)
	}
	if !got {
		t.Error("large watchlist missed its own last address")
	}
	got, err = e.Evaluate(context.Background(), watchlistEvent("transfer", false, wlDave, wlEve), params)
	if err != nil {
		t.Fatalf("evaluate miss: %v", err)
	}
	if got {
		t.Error("large watchlist matched an address not on it")
	}
}

func TestAddressWatchlistRejectsOversizedWatchlist(t *testing.T) {
	addrs := make([]string, MaxAddressWatchlistSize+1)
	for i := range addrs {
		addrs[i] = watchlistAddress(t, i)
	}
	paramsB, err := json.Marshal(map[string]any{"addresses": addrs})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	err = new(AddressWatchlist).Validate(paramsB)
	if err == nil {
		t.Fatal("Validate oversized watchlist = nil, want error")
	}
	if !contains(err.Error(), "the maximum is") {
		t.Errorf("Validate error %q, want a maximum-size message", err.Error())
	}
}

// TestAddressWatchlistSetReusedAcrossEvaluations checks the memoisation
// contract directly: after the first evaluation builds the set, identical
// params must hit the cache rather than rebuild the set per event.
func TestAddressWatchlistSetReusedAcrossEvaluations(t *testing.T) {
	e := new(AddressWatchlist)
	params := json.RawMessage(`{"addresses":["` + wlAlice + `"],"match":"from"}`)
	ev := watchlistEvent("transfer", false, wlAlice, wlBob)
	for i := 0; i < 3; i++ {
		got, err := e.Evaluate(context.Background(), ev, params)
		if err != nil {
			t.Fatalf("evaluate %d: %v", i, err)
		}
		if !got {
			t.Fatalf("evaluate %d = false, want true", i)
		}
	}
	if _, ok := e.sets.get(string(params)); !ok {
		t.Error("params not memoised after first evaluation")
	}
}

func BenchmarkAddressWatchlist(b *testing.B) {
	addrs := make([]string, 0, 256)
	for i := 0; i < 256; i++ {
		addrs = append(addrs, watchlistAddress(b, i))
	}
	paramsB, err := json.Marshal(map[string]any{"addresses": addrs})
	if err != nil {
		b.Fatalf("marshal params: %v", err)
	}
	ev := watchlistEvent("transfer", false, addrs[255], wlBob)
	e := new(AddressWatchlist)
	ctx := context.Background()
	b.Run("match", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := e.Evaluate(ctx, ev, json.RawMessage(paramsB))
			if err != nil {
				b.Fatal(err)
			}
			if !got {
				b.Fatal("got false, want true")
			}
		}
	})
}
