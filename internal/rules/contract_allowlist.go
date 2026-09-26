package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// contractAllowlistParams configure the contract_allowlist rule.
//
//	{
//	  "contract_ids": ["CDLZ...", "CBQH..."], // required: non-empty list of contract IDs
//	  "exclude": false                        // optional: invert into a denylist (default false)
//	}
//
// A monitor can watch several contracts while every rule on it applies to
// all of them; watching a protocol's ten contracts while alerting on large
// transfers for only the treasury contract would otherwise need a second
// monitor and a duplicated channel set. This rule is most useful inside a
// composite rule, AND-combined with the payload rule (e.g. a threshold or
// token_event rule) so one monitor says "large transfers, but only on the
// treasury contract". See the composite rule type for how to combine rules.
type contractAllowlistParams struct {
	ContractIDs []string `json:"contract_ids"`
	Exclude     bool     `json:"exclude"`
}

// ContractAllowlist matches when the event's contract ID is in (or, with
// exclude:true, not in) a configured list. It is stateless apart from a
// params-keyed set cache: identical params share the memoised set, so
// evaluating a long list costs a map lookup per event no matter how many
// contracts it holds.
type ContractAllowlist struct {
	sets allowlistSets
}

// validateContractList checks the contract_ids field shared by Validate and
// Evaluate, so both reject the same malformed params. IDs are checked with
// stellar.IsValidContractID, the same helper the monitor create path uses,
// rather than a second local check that could drift from it.
func validateContractList(p contractAllowlistParams) error {
	var details FieldErrors
	if len(p.ContractIDs) == 0 {
		details = append(details, FieldError{
			Field:  "contract_ids",
			Reason: "contract_allowlist: contract_ids is required (a non-empty list of contract IDs)",
		})
	}
	for i, id := range p.ContractIDs {
		if !stellar.IsValidContractID(id) {
			details = append(details, FieldError{
				Field:  fmt.Sprintf("contract_ids[%d]", i),
				Reason: fmt.Sprintf("contract_allowlist: contract_ids[%d] %q is not a valid contract ID", i, id),
			})
		}
	}
	if len(details) > 0 {
		return details
	}
	return nil
}

func (c *ContractAllowlist) Validate(params json.RawMessage) error {
	p, err := parseContractAllowlist(params)
	if err != nil {
		return err
	}
	return validateContractList(p)
}

func (c *ContractAllowlist) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	set, err := c.setFor(params)
	if err != nil {
		return false, err
	}
	_, inList := set.ids[ev.ContractID]
	if set.exclude {
		return !inList, nil
	}
	return inList, nil
}

// allowlistSet is the decoded, memoised form of a params blob: the contract
// set plus the derived exclude flag, so evaluation never re-parses the JSON
// or re-scans the ID list.
type allowlistSet struct {
	ids     map[string]struct{}
	exclude bool
}

// allowlistSets memoises an allowlistSet per params blob. The poller
// evaluates the same rule params once per event, and rebuilding the set per
// event would defeat the point of the rule; the cache is keyed by the raw
// params, capped, and safe for concurrent use — the poller evaluates rules
// concurrently across monitors. It follows TopicRegex's regexCache, down to
// the shared regexCacheMax cap: one entry per distinct rule params, and
// rules are deleted along with their monitors, so a small cap is plenty and
// an eviction merely loses an optimisation.
type allowlistSets struct {
	mu sync.RWMutex
	m  map[string]allowlistSet
}

func (c *allowlistSets) get(params string) (allowlistSet, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.m[params]
	return s, ok
}

func (c *allowlistSets) put(params string, s allowlistSet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]allowlistSet, 8)
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
func (c *ContractAllowlist) setFor(params json.RawMessage) (allowlistSet, error) {
	if s, ok := c.sets.get(string(params)); ok {
		return s, nil
	}
	p, err := parseContractAllowlist(params)
	if err != nil {
		return allowlistSet{}, err
	}
	if err := validateContractList(p); err != nil {
		return allowlistSet{}, err
	}
	s := allowlistSet{ids: make(map[string]struct{}, len(p.ContractIDs)), exclude: p.Exclude}
	for _, id := range p.ContractIDs {
		s.ids[id] = struct{}{}
	}
	c.sets.put(string(params), s)
	return s, nil
}

func parseContractAllowlist(params json.RawMessage) (contractAllowlistParams, error) {
	var p contractAllowlistParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("contract_allowlist: invalid params: %w", err)
	}
	return p, nil
}
