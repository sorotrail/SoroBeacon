// Package poller ingests Soroban contract events from a Stellar RPC node
// and feeds them through the rules engine, creating and dispatching alerts.
package poller

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// Position is a race-free snapshot of how far the poller has got relative
// to the chain. A zero LastSuccessfulPoll means no successful poll has
// completed yet — callers must not treat the ledger fields as "in sync".
type Position struct {
	LastProcessedLedger uint32
	LatestChainLedger   uint32
	LastSuccessfulPoll  time.Time
	BackingOff          bool
}

// Ready reports whether a successful poll has completed.
func (p Position) Ready() bool { return !p.LastSuccessfulPoll.IsZero() }

// Lag is latest known chain ledger minus last processed ledger.
func (p Position) Lag() int64 {
	return int64(p.LatestChainLedger) - int64(p.LastProcessedLedger)
}

// Store is the slice of the store the poller needs.
type Store interface {
	ListMonitors(ctx context.Context, enabledOnly bool) ([]store.Monitor, error)
	ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error)
	CreateAlert(ctx context.Context, a *store.Alert) (store.AlertOutcome, error)
	GetIngestState(ctx context.Context) (store.IngestState, error)
	SetIngestState(ctx context.Context, s store.IngestState) error
	// The ledger-hash window backing reorg detection, and the retraction
	// write that marks alerts orphaned by a reorg.
	RecordLedgerHashes(ctx context.Context, hashes []store.LedgerHash) error
	LedgerHashes(ctx context.Context, from, to uint32) ([]store.LedgerHash, error)
	PruneLedgerHashes(ctx context.Context, before uint32) error
	RetractAlertsFromLedger(ctx context.Context, ledger uint32, at time.Time) (int64, error)
}

// Dispatcher receives every newly created alert. Implemented by
// notify.Dispatcher; mocked in tests.
type Dispatcher interface {
	Dispatch(ctx context.Context, a notify.Alert)
}

// Poller runs the ingest loop: getEvents from the last checkpoint, decode,
// match rules, create + dispatch alerts, advance the checkpoint.
type Poller struct {
	source   EventSource
	store    Store
	registry *rules.Registry
	interval time.Duration
	log      *slog.Logger
	// ing evaluates events and records alerts; shared with the backfill job
	// so a historical replay and live ingestion match identically.
	ing *Ingestor
	// metrics is optional instrumentation; nil-safe, see internal/metrics.
	metrics *metrics.Metrics
	// scanned/matched accumulate per-cycle counts for metrics.
	scanned int
	matched int
	// pos is the last successful poll snapshot, stored as Position.
	// atomic.Value so HTTP handlers can read it without a mutex.
	pos atomic.Value
	// sched orders each cycle's watch list by monitor priority. It carries
	// rotation state between cycles, so a contract cannot be permanently
	// last within its tier.
	sched *Scheduler
	// reorgWindow is how many recent ledgers' hashes are kept and re-checked
	// each cycle. Zero disables reorg detection (the pre-feature behaviour).
	reorgWindow uint32
	// confirmDepth is how many ledgers behind the tip an event must be before
	// it may alert. Zero alerts immediately, which is the historical default.
	confirmDepth uint32
}

// Position returns the last successful poll snapshot. Safe to call from
// another goroutine (the HTTP health handler). Before the first successful
// poll the returned Position is the zero value and Ready is false.
func (p *Poller) Position() Position {
	if v := p.pos.Load(); v != nil {
		return v.(Position)
	}
	return Position{}
}

func (p *Poller) recordPosition(processed, latest uint32, at time.Time) {
	p.pos.Store(Position{
		LastProcessedLedger: processed,
		LatestChainLedger:   latest,
		LastSuccessfulPoll:  at.UTC(),
	})
}

func (p *Poller) recordBackoff(backingOff bool) {
	position := p.Position()
	position.BackingOff = backingOff
	p.pos.Store(position)
}

// New wires a Poller. src is where events come from: NewRPCSource for a
// Stellar RPC node, or the SoroTrail source for upstream mode.
func New(src EventSource, st Store, reg *rules.Registry, d Dispatcher, interval time.Duration, log *slog.Logger) *Poller {
	return &Poller{
		source:   src,
		store:    st,
		registry: reg,
		interval: interval,
		log:      log,
		ing:      NewIngestor(st, reg, d, log),
		sched:    NewScheduler(),
	}
}

// WithMetrics attaches Prometheus instrumentation to the poll loop.
func (p *Poller) WithMetrics(m *metrics.Metrics) *Poller {
	p.metrics = m
	p.ing = p.ing.WithMetrics(m)
	return p
}

// WithReorg enables reorg detection over a window of `window` recent ledgers
// and holds alerts until they are `depth` ledgers behind the tip. window 0
// disables detection and depth 0 alerts immediately: both defaults reproduce
// the behaviour before reorg handling existed. Sources that cannot report
// ledger hashes are tolerated — detection simply does not run.
func (p *Poller) WithReorg(window, depth uint32) *Poller {
	p.reorgWindow = window
	p.confirmDepth = depth
	return p
}

// Run polls until ctx is cancelled. RPC errors back off exponentially
// (capped at 10x the poll interval) instead of hammering the node.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("poller started", "interval", p.interval)
	delay := p.interval
	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopped")
			return
		case <-time.After(delay):
		}

		p.scanned, p.matched = 0, 0
		start := time.Now()
		err := p.Poll(ctx)
		p.metrics.RecordPoll(err == nil, time.Since(start))
		p.metrics.RecordEvents(p.scanned, p.matched)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			delay = min(delay*2, 10*p.interval)
			p.recordBackoff(true)
			p.log.Error("poll failed", "err", err, "retry_in", delay)
			continue
		}
		delay = p.interval
		p.recordBackoff(false)
	}
}

// Poll runs one ingest cycle. Exported so tests (and one-shot tools) can
// drive the poller without the timing loop.
func (p *Poller) Poll(ctx context.Context) error {
	monitors, err := p.store.ListMonitors(ctx, true)
	if err != nil {
		return err
	}

	// Map each contract to the monitors watching it; dedupe contracts. Along
	// the way, collect the event names its enabled rules require so the source
	// can push a topic filter into getEvents instead of streaming events we
	// would immediately discard. A contract is only narrowed when every rule
	// watching it names a concrete event; otherwise it stays unfiltered.
	byContract := map[string][]store.Monitor{}
	var contracts []string
	namesByContract := map[string]map[string]bool{}
	unfilterable := map[string]bool{}
	// prioByContract is the highest priority among the monitors watching a
	// contract: a contract qualifies for the earliest tier it is named in,
	// so a high-priority monitor is never slowed by sharing a contract with
	// a low-priority one.
	prioByContract := map[string]store.Priority{}
	for _, m := range monitors {
		ruleList, err := p.store.ListRules(ctx, m.ID, true)
		if err != nil {
			return err
		}
		names, ok := ruleEventNames(p.registry, ruleList)
		for _, c := range m.ContractIDs {
			// A single malformed ID makes the RPC reject the whole request,
			// stalling ingestion for every monitor — skip, don't send.
			if !stellar.IsValidContractID(c) {
				p.log.Warn("skipping invalid contract id", "monitor_id", m.ID, "contract_id", c)
				continue
			}
			if _, seen := byContract[c]; !seen {
				contracts = append(contracts, c)
				prioByContract[c] = m.Priority.Normalized()
			} else if rank := m.Priority.Rank(); rank > prioByContract[c].Rank() {
				prioByContract[c] = m.Priority.Normalized()
			}
			byContract[c] = append(byContract[c], m)
			if unfilterable[c] {
				continue
			}
			if !ok {
				unfilterable[c] = true
				continue
			}
			if namesByContract[c] == nil {
				namesByContract[c] = map[string]bool{}
			}
			for _, n := range names {
				namesByContract[c][n] = true
			}
		}
	}
	if len(contracts) == 0 {
		return nil
	}

	// Compile the derived filters into the watch list the source sees, ordered
	// by priority so a high-priority contract is fetched in an earlier request
	// instead of waiting behind a batch of low-traffic ones. A nil Topics is
	// the safe default: no server-side narrowing.
	scheduled := make([]scheduledContract, 0, len(contracts))
	tierCounts := map[store.Priority]int{}
	for _, c := range contracts {
		sc := scheduledContract{ContractID: c, Priority: prioByContract[c].Normalized()}
		if !unfilterable[c] {
			sc.Topics = topicFiltersFor(sortedKeys(namesByContract[c]))
		}
		scheduled = append(scheduled, sc)
		tierCounts[sc.Priority]++
	}
	watch := p.sched.Order(scheduled)
	for _, pr := range prioritiesInServiceOrder {
		p.metrics.SetPriorityContracts(string(pr), tierCounts[pr])
	}

	state, err := p.store.GetIngestState(ctx)
	if err != nil {
		return err
	}
	startLedger := state.LastLedger + 1
	if state.LastLedger == 0 {
		// Cold start against an RPC source: the node only retains ~1-7
		// days of events, so begin at the current tip. Upstream sources
		// (SoroTrail) hold durable history and answer the same query.
		latest, err := p.source.LatestLedger(ctx)
		if err != nil {
			return err
		}
		startLedger = latest
		p.log.Info("cold start", "start_ledger", startLedger)
	}

	// Detect a reorganisation before ingesting, so alerts derived from the
	// orphaned ledgers are retracted before the replacement chain's events are
	// evaluated. A detected divergence also rewinds the checkpoint, so the
	// new chain is re-read even though the old one had advanced past it.
	if divergence, err := p.detectReorg(ctx); err != nil {
		p.log.Warn("reorg check failed", "err", err)
	} else if divergence != 0 && divergence-1 < state.LastLedger {
		state.LastLedger = divergence - 1
		state.LastCursor = ""
		if err := p.store.SetIngestState(ctx, state); err != nil {
			return err
		}
		startLedger = state.LastLedger + 1
		p.log.Warn("rewinding checkpoint after reorg", "start_ledger", startLedger)
	}

	// Confirmation depth: hold events until they are `confirmDepth` ledgers
	// behind the tip. Unconfirmed events are simply not evaluated now; the
	// capped checkpoint means the same range is re-read once it is confirmed.
	var confirmedThrough uint32
	confirmActive := p.confirmDepth > 0
	if confirmActive {
		tipNow, err := p.source.LatestLedger(ctx)
		if err != nil {
			return err
		}
		if tipNow > p.confirmDepth {
			confirmedThrough = tipNow - p.confirmDepth
		}
	}

	// Page the source until it reports no more events for the cycle. The
	// cursor is opaque; batching (the RPC caps filters per request) is the
	// source's concern, encoded in its cursors.
	checkpoint := uint32(0) // min latestLedger across pages
	tip := uint32(0)        // max latestLedger across pages
	// tierLedger records the highest ledger at which an event was seen for
	// each tier, so per-tier lag reflects how stale the newest data that tier
	// has produced is. A tier with no events falls back to the checkpoint.
	tierLedger := map[store.Priority]uint32{}
	cursor := ""
	for {
		page, err := p.source.FetchEvents(ctx, startLedger, watch, cursor, stellar.DefaultEventsLimit)
		if err != nil {
			return err
		}
		if page.LatestLedger > 0 && (checkpoint == 0 || page.LatestLedger < checkpoint) {
			checkpoint = page.LatestLedger
		}
		if page.LatestLedger > tip {
			tip = page.LatestLedger
		}
		p.scanned += len(page.Events)
		for _, ev := range page.Events {
			if confirmActive && ev.Ledger > confirmedThrough {
				continue // not yet buried deep enough; re-read once it is
			}
			if pr, ok := prioByContract[ev.ContractID]; ok && ev.Ledger > tierLedger[pr] {
				tierLedger[pr] = ev.Ledger
			}
			p.handleEvent(ctx, ev, byContract)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	// Lag: how far the checkpoint we reached trails the node's own tip.
	// Grows when a batch's page-through takes longer than ledger cadence.
	if confirmActive && checkpoint > confirmedThrough {
		checkpoint = confirmedThrough
	}
	p.metrics.SetPollLag(int64(tip) - int64(checkpoint))
	for _, pr := range prioritiesInServiceOrder {
		if tierCounts[pr] == 0 {
			continue
		}
		observed, ok := tierLedger[pr]
		if !ok {
			observed = checkpoint
		}
		p.metrics.SetPollLagByPriority(string(pr), int64(tip)-int64(observed))
	}
	if checkpoint > state.LastLedger {
		state.LastLedger = checkpoint
		state.LastCursor = ""
		if err := p.store.SetIngestState(ctx, state); err != nil {
			return err
		}
	}
	processed := checkpoint
	if processed == 0 {
		processed = state.LastLedger
	}
	p.recordPosition(processed, tip, time.Now())
	return nil
}

// handleEvent runs every enabled rule of every monitor watching the event's
// contract. Events arrive already decoded from the source; the shared
// Ingestor owns evaluation, alert persistence and dispatch.
func (p *Poller) handleEvent(ctx context.Context, decoded *stellar.DecodedEvent, byContract map[string][]store.Monitor) {
	monitors, watched := byContract[decoded.ContractID]
	if !watched {
		return
	}
	p.matched += p.ing.Handle(ctx, decoded, monitors, HandleOptions{Deliver: true}).Matched
}
