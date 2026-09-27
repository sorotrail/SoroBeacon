package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/stellar/go-stellar-sdk/strkey"
)

// MaxAddressWatchlistSize bounds a watchlist's address list. Validate
// rejects anything larger, so an accidental paste of an entire ledger's
// worth of addresses is rejected at create time by the API instead of
// ballooning the rule's stored params. The cap sits far above the
// hundreds-of-addresses watchlists the rule is built for.
const MaxAddressWatchlistSize = 1024

// addressWatchlistParams configure the address_watchlist rule.
//
//	{
//	  "addresses": ["GA...", "GB..."], // required: non-empty list of Stellar addresses
//	  "match": "either",               // optional: from|to|either (default either)
//	  "event": "transfer"              // optional: restrict to one SEP-41 event name
//	}
//
// The most common real monitoring question after "did this token move" is
// "did these specific addresses move anything". Until now that needed one
// token_event rule per address, which did not scale past a handful and made
// the monitor page unreadable; a watchlist rule collapses it into one rule.
//
// The rule reuses token_event's SEP-41 topic layout (sep41Events) and slot
// decoding (addr), so it sees the same semantic slots: on burn and clawback
// the outgoing slot is the holder, not an admin. Matching is exact and
// case-sensitive — Stellar strkeys are case-sensitive, so no partial or
// prefix matching.
type addressWatchlistParams struct {
	Addresses []string `json:"addresses"`
	Match     string   `json:"match"`
	Event     string   `json:"event"`
}

// AddressWatchlist matches SEP-41 token events whose from or to address
// appears in a configured watchlist. It is stateless apart from a
// params-keyed set cache: identical params share the memoised set, so
// evaluating hundreds of addresses costs a map lookup per event no matter
// how long the list is.
type AddressWatchlist struct {
	sets watchlistSets
}

// validateAddressList checks the addresses and match fields shared by
// Validate and Evaluate, so both reject the same malformed params.
func validateAddressList(p addressWatchlistParams) error {
	var details FieldErrors
	if len(p.Addresses) == 0 {
		details = append(details, FieldError{
			Field:  "addresses",
			Reason: "address_watchlist: addresses is required (a non-empty list of Stellar addresses)",
		})
	}
	for i, a := range p.Addresses {
		if !isStellarAddress(a) {
			details = append(details, FieldError{
				Field:  fmt.Sprintf("addresses[%d]", i),
				Reason: fmt.Sprintf("address_watchlist: addresses[%d] %q is not a plausible Stellar address", i, a),
			})
		}
	}
	if len(p.Addresses) > MaxAddressWatchlistSize {
		details = append(details, FieldError{
			Field:  "addresses",
			Reason: fmt.Sprintf("address_watchlist: addresses has %d entries; the maximum is %d", len(p.Addresses), MaxAddressWatchlistSize),
		})
	}
	switch p.Match {
	case "", "from", "to", "either":
	default:
		details = append(details, FieldError{
			Field:  "match",
			Reason: fmt.Sprintf("address_watchlist: unknown match %q (want from|to|either)", p.Match),
		})
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

func (a *AddressWatchlist) Validate(params json.RawMessage) error {
	p, err := parseAddressWatchlist(params)
	if err != nil {
		return err
	}
	if err := validateAddressList(p); err != nil {
		return err
	}
	// An optional event restriction is checked here, at create time, because
	// a misspelled name would otherwise silently never match — the poller
	// has no way to tell a typo from an event the contract has not emitted
	// yet.
	if p.Event != "" && p.Event != "*" {
		if _, ok := sep41Events[p.Event]; !ok {
			return FieldError{
				Field:  "event",
				Reason: fmt.Sprintf("address_watchlist: unknown event %q (want transfer|mint|burn|clawback|set_admin|*)", p.Event),
			}
		}
	}
	return nil
}

func (a *AddressWatchlist) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	set, err := a.setFor(params)
	if err != nil {
		return false, err
	}
	if len(ev.Topics) < 3 {
		return false, nil
	}
	name := ev.EventName()
	slots, isSep41 := sep41Events[name]
	if !isSep41 {
		return false, nil
	}
	// "*" and "" (unrestricted) are the same thing at evaluation time.
	if set.event != "" && set.event != "*" && name != set.event {
		return false, nil
	}
	// The SEP-41 topic layout puts both address slots at fixed positions, so
	// which slots to consult depends only on match — not on the event name.
	if set.match != "to" && set.contains(addr(ev.Topics[slots[0]])) {
		return true, nil
	}
	if set.match != "from" && set.contains(addr(ev.Topics[slots[1]])) {
		return true, nil
	}
	return false, nil
}

// EventNames reports the SEP-41 events this rule is scoped to, mirroring
// TokenEvent's EventNamer: an unrestricted rule may match any SEP-41 event
// and reports them all; a rule restricted to one name reports that name, so
// the poller can narrow its server-side getEvents filter.
func (a *AddressWatchlist) EventNames(params json.RawMessage) ([]string, bool) {
	p, err := parseAddressWatchlist(params)
	if err != nil {
		return nil, false
	}
	if p.Event == "" || p.Event == "*" {
		names := make([]string, 0, len(sep41Events))
		for name := range sep41Events {
			names = append(names, name)
		}
		return names, true
	}
	return []string{p.Event}, true
}

// watchlistSet is the decoded, memoised form of a params blob: the address
// set plus the derived match mode and event restriction, so evaluation
// never re-parses the JSON or re-scans the address list.
type watchlistSet struct {
	addresses map[string]struct{}
	match     string
	event     string
}

// contains reports whether an address is on the watchlist.
func (s watchlistSet) contains(address string) bool {
	_, ok := s.addresses[address]
	return ok
}

// watchlistSets memoises a watchlistSet per params blob. The poller
// evaluates the same rule params once per event, and rebuilding a
// hundreds-entry set per event would defeat the point of the rule; the
// cache is keyed by the raw params, capped, and safe for concurrent use —
// the poller evaluates rules concurrently across monitors. It follows
// TopicRegex's regexCache, down to the shared regexCacheMax cap: one entry
// per distinct rule params, and rules are deleted along with their
// monitors, so a small cap is plenty and an eviction merely loses an
// optimisation.
type watchlistSets struct {
	mu sync.RWMutex
	m  map[string]watchlistSet
}

func (c *watchlistSets) get(params string) (watchlistSet, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.m[params]
	return s, ok
}

func (c *watchlistSets) put(params string, s watchlistSet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]watchlistSet, 8)
	}
	if len(c.m) >= regexCacheMax {
		// Drop one arbitrary entry; correctness does not depend on which.
		for k := range c.m {
			delete(c.m, k)
			break
		}
	}
	c.m[params] = s
}

// setFor returns the memoised set for a params blob, revalidating on every
// cache miss: the same checks Validate runs run here too, so malformed
// params fail identically at rule-create time and in the poller.
func (a *AddressWatchlist) setFor(params json.RawMessage) (watchlistSet, error) {
	if s, ok := a.sets.get(string(params)); ok {
		return s, nil
	}
	p, err := parseAddressWatchlist(params)
	if err != nil {
		return watchlistSet{}, err
	}
	if err := validateAddressList(p); err != nil {
		return watchlistSet{}, err
	}
	match := p.Match
	if match == "" {
		match = "either"
	}
	s := watchlistSet{addresses: make(map[string]struct{}, len(p.Addresses)), match: match, event: p.Event}
	for _, a := range p.Addresses {
		s.addresses[a] = struct{}{}
	}
	a.sets.put(string(params), s)
	return s, nil
}

// isStellarAddress reports whether s is a plausible Stellar address: a
// well-formed account (G...) or contract (C...) strkey. Muxed accounts,
// liquidity pools and claimable balances are deliberately excluded —
// SEP-41 events address accounts and token contracts, and the narrower
// check keeps a mistyped M.../L... strkey from silently never matching.
func isStellarAddress(s string) bool {
	if strkey.IsValidEd25519PublicKey(s) {
		return true
	}
	_, err := strkey.Decode(strkey.VersionByteContract, s)
	return err == nil
}

func parseAddressWatchlist(params json.RawMessage) (addressWatchlistParams, error) {
	var p addressWatchlistParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("address_watchlist: invalid params: %w", err)
	}
	return p, nil
}
