package poller

import "github.com/sorotrail/sorobeacon/internal/store"

// scheduledContract is one contract in a cycle's watch list, carrying the
// priority derived from the monitors watching it. The scheduler orders these
// before the source batches them, so the priority decides request order
// rather than the map iteration order the watch list used to inherit.
type scheduledContract struct {
	ContractID string
	Topics     [][]string
	Priority   store.Priority
}

// prioritiesInServiceOrder is the fixed tier order the scheduler walks:
// highest first, so a high-priority contract is always queued ahead of a
// normal or low one. It is the one place the tier ordering is defined.
var prioritiesInServiceOrder = []store.Priority{
	store.PriorityHigh,
	store.PriorityNormal,
	store.PriorityLow,
}

// tierWeight is how many contracts a tier contributes per round of the
// weighted round-robin. Every tier's weight is at least one, so every tier is
// represented in the first round: the lowest-priority contract can never be
// pushed further back than the sum of the weights (1 + 2 + 4 = 7 contracts),
// no matter how many high-priority contracts are queued. That bound is what
// makes strict priority safe to ship.
func tierWeight(p store.Priority) int {
	switch p {
	case store.PriorityHigh:
		return 4
	case store.PriorityLow:
		return 1
	default:
		return 2
	}
}

// Scheduler turns a cycle's watch list into an ordered request plan. It keeps
// one rotation cursor per tier so the contract that happens to sort first in
// a tier is not permanently served first, which would starve its peers even
// though the tier itself is never starved.
//
// The zero value is not usable; construct one with NewScheduler.
type Scheduler struct {
	cursor map[store.Priority]int
}

// NewScheduler returns a Scheduler with its rotation cursors at zero.
func NewScheduler() *Scheduler {
	return &Scheduler{cursor: map[store.Priority]int{}}
}

// Order returns the cycle's watch list ordered high-to-low priority using
// weighted round-robin. Every input contract appears exactly once, so the
// plan can never drop a contract — it only decides who is polled first.
func (s *Scheduler) Order(items []scheduledContract) []Watch {
	buckets := map[store.Priority][]scheduledContract{}
	for _, it := range items {
		p := it.Priority.Normalized()
		buckets[p] = append(buckets[p], it)
	}

	// Rotate each tier's queue left by its cursor. The size is captured
	// before consumption so the cursor advances relative to the original
	// length, and a tier with no contracts keeps its cursor at zero.
	sizes := make(map[store.Priority]int, len(buckets))
	for _, p := range prioritiesInServiceOrder {
		b := buckets[p]
		sizes[p] = len(b)
		if len(b) == 0 {
			continue
		}
		c := s.cursor[p] % len(b)
		buckets[p] = append(append([]scheduledContract(nil), b[c:]...), b[:c]...)
	}

	var out []Watch
	for {
		emitted := false
		for _, p := range prioritiesInServiceOrder {
			b := buckets[p]
			if len(b) == 0 {
				continue
			}
			n := min(tierWeight(p), len(b))
			for _, it := range b[:n] {
				out = append(out, Watch{ContractID: it.ContractID, Topics: it.Topics})
			}
			buckets[p] = b[n:]
			emitted = true
		}
		if !emitted {
			break
		}
	}

	// Advance each tier's cursor so the next cycle starts one contract later.
	for _, p := range prioritiesInServiceOrder {
		if sizes[p] > 0 {
			s.cursor[p] = (s.cursor[p] + 1) % sizes[p]
		}
	}
	return out
}
