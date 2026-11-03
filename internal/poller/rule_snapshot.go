package poller

import (
	"context"
	"reflect"
	"sync"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type cycleRuleSnapshot struct {
	batchMap  map[string][]store.Monitor
	batchAddr uintptr
	rules     map[int64][]store.Rule
}

var (
	cycleRuleSnapshotsMu sync.Mutex
	cycleRuleSnapshots   = map[*Poller]*cycleRuleSnapshot{}
)

// listRulesForCycle returns the enabled rules for monitorID, snapshotted once
// per poll cycle (identified by the per-cycle byContract map allocated in
// Poll). Every event in the same poll batch is evaluated against a consistent
// rule snapshot, while the next poll cycle allocates a new map and loads fresh
// rules from the store.
func (p *Poller) listRulesForCycle(ctx context.Context, byContract map[string][]store.Monitor, monitorID int64) ([]store.Rule, error) {
	if byContract == nil {
		return p.store.ListRules(ctx, monitorID, true)
	}
	addr := reflect.ValueOf(byContract).Pointer()

	cycleRuleSnapshotsMu.Lock()
	snap := cycleRuleSnapshots[p]
	if snap == nil || snap.batchAddr != addr {
		snap = &cycleRuleSnapshot{
			batchMap:  byContract,
			batchAddr: addr,
			rules:     make(map[int64][]store.Rule),
		}
		cycleRuleSnapshots[p] = snap
	}
	if cached, ok := snap.rules[monitorID]; ok {
		out := make([]store.Rule, len(cached))
		copy(out, cached)
		cycleRuleSnapshotsMu.Unlock()
		return out, nil
	}
	cycleRuleSnapshotsMu.Unlock()

	ruleList, err := p.store.ListRules(ctx, monitorID, true)
	if err != nil {
		return nil, err
	}

	cycleRuleSnapshotsMu.Lock()
	if cur := cycleRuleSnapshots[p]; cur != nil && cur.batchAddr == addr {
		cloned := make([]store.Rule, len(ruleList))
		copy(cloned, ruleList)
		cur.rules[monitorID] = cloned
	}
	cycleRuleSnapshotsMu.Unlock()

	return ruleList, nil
}
