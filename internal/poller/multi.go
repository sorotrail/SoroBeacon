package poller

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Unit is one network's ingest pipeline as the supervisor sees it: a poller
// already scoped to one network by WithNetwork, so it holds that chain's own
// source, cursor and backoff state.
type Unit struct {
	Network string
	Poller  *Poller
}

// Supervisor runs one poller per network in one process.
//
// It exists because the alternative — one instance per network — does not
// work for a monitor that tracks the same contract on testnet and mainnet:
// the two are different chains that happen to share an address, and an
// operator wants one place to say which one alerted. Polling several chains
// from one process only holds up if one chain's failure cannot affect
// another's, so every part of this design is about isolation: separate
// goroutines, separate cursors (see the store's per-network ingest state),
// separate exponential backoff (it lives inside each Poller's own Run), and a
// recovered panic that restarts one loop instead of killing the process.
//
// A single-network deployment does not need it: Poller.Run on its own is
// what that looks like, and it stays that way.
type Supervisor struct {
	units []Unit
	log   *slog.Logger
	// started is when Run began, so Statuses can tell "no poll yet" from
	// "polling stopped": a unit whose Position is empty shortly after start
	// is warming up, and one that is empty an hour later has stopped.
	started time.Time
}

// NewSupervisor wires a supervisor over units, which must arrive primary
// first: the first entry is the one whose lag the aggregate position reports,
// and the order dashboards list networks in.
func NewSupervisor(log *slog.Logger, units ...Unit) *Supervisor {
	return &Supervisor{units: units, log: log, started: time.Now()}
}

// Networks lists the configured network names, primary first.
func (s *Supervisor) Networks() []string {
	names := make([]string, 0, len(s.units))
	for _, u := range s.units {
		names = append(names, u.Network)
	}
	return names
}

// Run starts every unit's poll loop and returns when ctx is cancelled.
//
// Each loop waits on ctx itself, so cancelling stops all of them; this
// function only has to hold the goroutines alive for the process's lifetime,
// which is what makes `go sup.Run(ctx)` behave exactly like the single
// `go p.Run(ctx)` it replaced.
func (s *Supervisor) Run(ctx context.Context) {
	// One unit — the primary — is told the full network list so it can report
	// monitors on a chain nobody polls. Naming a single reporter keeps the
	// warning from appearing once per network, and more than one unit means
	// the networks genuinely are isolated: a monitor on a chain outside this
	// list is dead, not merely waiting for its sibling.
	if len(s.units) > 1 {
		s.units[0].Poller.knownNetworks = s.Networks()
	}
	var wg sync.WaitGroup
	for _, u := range s.units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runUnit(ctx, u)
		}()
	}
	<-ctx.Done()
	wg.Wait()
}

// runUnit keeps one network ingesting until ctx is cancelled, restarting the
// loop if it ever ends by panicking.
//
// The recover is the point of this function. An unrecovered panic stops the
// whole process, so without it one network's malformed event would stop every
// other network's ingestion too — the exact coupling the supervisor exists to
// prevent. Restarting after the poll interval rather than immediately keeps a
// panic caused by stored state from hot-looping.
func (s *Supervisor) runUnit(ctx context.Context, u Unit) {
	for {
		if !s.runGuarded(ctx, u) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.Poller.interval):
		}
	}
}

// runGuarded runs one poll loop to completion, reporting whether it ended by
// panicking (true) or because ctx was cancelled (false).
func (s *Supervisor) runGuarded(ctx context.Context, u Unit) (crashed bool) {
	log := s.log.With("network", u.Network)
	defer func() {
		if v := recover(); v != nil {
			// The stack is the actionable half of this line: a panic deep in
			// event decoding is unfindable without it, and it costs nothing
			// because this only runs when something already went wrong.
			log.Error("poller panicked; restarting its loop",
				"panic", v, "stack", string(debug.Stack()))
			u.Poller.metrics.RecordPanic()
			crashed = true
		}
	}()
	log.Info("network poller started")
	u.Poller.Run(ctx)
	return false
}

// Positions snapshots every unit's ingest position, primary first. The
// dashboard and /health read this to show per-network progress.
func (s *Supervisor) Positions() []Position {
	out := make([]Position, 0, len(s.units))
	for _, u := range s.units {
		out = append(out, u.Poller.Position())
	}
	return out
}

// Position reports the aggregate position of every network, which is the
// worst one: the most behind. This is what lets the supervisor satisfy the
// same PositionReader interface a single Poller does, so /readyz keeps failing
// on lag no matter which chain is lagging.
//
// Summing the ledgers would be wrong — chains number ledgers independently, so
// a combined "last processed" is not a number anyone can act on — and
// reporting the primary alone would hide a secondary that stopped entirely.
// The last-poll timestamp is the oldest across networks, so a
// seconds-since-last-poll alert fires for the stalest chain.
func (s *Supervisor) Position() Position {
	agg := Position{Network: "aggregate", LastSuccessfulPoll: s.started}
	var worst Position
	have := false
	for _, u := range s.units {
		pos := u.Poller.Position()
		if !pos.Ready() {
			// A network that has not completed a poll is the most behind
			// there is, and reporting the aggregate as ready would let
			// /readyz pass while half the instance ingests nothing.
			agg.LastProcessedLedger, agg.LatestChainLedger = 0, 0
			agg.LastSuccessfulPoll = time.Time{}
			return agg
		}
		if !have || pos.Lag() > worst.Lag() {
			worst, have = pos, true
		}
		if pos.LastSuccessfulPoll.Before(agg.LastSuccessfulPoll) {
			agg.LastSuccessfulPoll = pos.LastSuccessfulPoll
		}
	}
	if have {
		agg.LastProcessedLedger = worst.LastProcessedLedger
		agg.LatestChainLedger = worst.LatestChainLedger
	}
	return agg
}

// NetworkStatus is one network's ingest and source health. The health
// endpoint reports an array of these instead of collapsing several chains
// into one boolean, because "is SoroBeacon healthy?" has a different answer
// per chain and an operator needs to know which one to fix.
type NetworkStatus struct {
	Network string `json:"network"`
	// Source is "ok", or the error that reaching the chain's tip returned.
	// The same shape as the /health endpoint's existing "rpc" field.
	Source              string `json:"source"`
	LastProcessedLedger uint32 `json:"last_processed_ledger"`
	LatestChainLedger   uint32 `json:"latest_chain_ledger"`
	LedgerLag           int64  `json:"ledger_lag"`
	// LastPollAt is RFC3339 as the API renders every other timestamp, and
	// empty until this network has completed a poll.
	LastPollAt string `json:"last_poll_at,omitempty"`
}

// Statuses asks each network's source for its current tip and combines that
// with the poller's position. A source that fails reports its chain as down
// and leaves the other networks' entries intact — asking whether network N is
// healthy must never be blocked by network M's node being unreachable.
//
// ctx bounds the sweep; callers pass a request context with a timeout.
func (s *Supervisor) Statuses(ctx context.Context) []NetworkStatus {
	out := make([]NetworkStatus, 0, len(s.units))
	for _, u := range s.units {
		st := NetworkStatus{Network: u.Network, Source: "ok"}
		pos := u.Poller.Position()
		st.LastProcessedLedger = pos.LastProcessedLedger
		st.LatestChainLedger = pos.LatestChainLedger
		if pos.Ready() {
			st.LastPollAt = pos.LastSuccessfulPoll.UTC().Format(time.RFC3339)
		}
		tip, err := u.Poller.source.LatestLedger(ctx)
		if err != nil {
			st.Source = err.Error()
			// Without a live tip the last one the poller saw is the only
			// honest lag: reporting 0 would read as "caught up" for a chain
			// whose node is unreachable.
			st.LedgerLag = pos.Lag()
		} else {
			st.LatestChainLedger = tip
			st.LedgerLag = int64(tip) - int64(pos.LastProcessedLedger)
		}
		out = append(out, st)
	}
	return out
}
