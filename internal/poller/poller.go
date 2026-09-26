// Package poller ingests Soroban contract events from a Stellar RPC node
// and feeds them through the rules engine, creating and dispatching alerts.
package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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
	// Network is the chain this snapshot describes, "" for an instance that
	// was never scoped to one. Lag is only comparable within one network:
	// each chain produces ledgers on its own schedule, so a public-network
	// lag of 5 and a futurenet lag of 40 are different amounts of lateness.
	Network             string
	LastProcessedLedger uint32
	LatestChainLedger   uint32
	LastSuccessfulPoll  time.Time
}

// Ready reports whether a successful poll has completed.
func (p Position) Ready() bool { return !p.LastSuccessfulPoll.IsZero() }

// Lag is latest known chain ledger minus last processed ledger.
func (p Position) Lag() int64 {
	return int64(p.LatestChainLedger) - int64(p.LastProcessedLedger)
}

// Store is the slice of the store the poller needs.
//
// The ingest and reorg-window methods take this poller's network. Each chain
// keeps its own cursor and its own ledger-hash window, so two pollers running
// concurrently cannot rewind each other, and a reorg on one chain cannot
// retract the other chain's alerts.
type Store interface {
	ListMonitors(ctx context.Context, enabledOnly bool) ([]store.Monitor, error)
	ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error)
	CreateAlert(ctx context.Context, a *store.Alert) (store.AlertOutcome, error)
	GetIngestState(ctx context.Context, network string) (store.IngestState, error)
	SetIngestState(ctx context.Context, network string, s store.IngestState) error
	// The ledger-hash window backing reorg detection, and the retraction
	// write that marks alerts orphaned by a reorg.
	RecordLedgerHashes(ctx context.Context, network string, hashes []store.LedgerHash) error
	LedgerHashes(ctx context.Context, network string, from, to uint32) ([]store.LedgerHash, error)
	PruneLedgerHashes(ctx context.Context, network string, before uint32) error
	RetractAlertsFromLedger(ctx context.Context, network string, ledger uint32, at time.Time) (int64, error)
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
	dispatch Dispatcher
	interval time.Duration
	log      *slog.Logger
	// network is the Stellar network this poller ingests: the only monitors it
	// watches, the only checkpoint it advances and the only reorg window it
	// reads. Empty means "the pre-multi-network instance": every monitor, and
	// the legacy single-row ingest state — which is what keeps every existing
	// caller and test of New unchanged.
	network string
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
	// knownNetworks, set only on the supervisor's reporting unit, is every
	// network this instance polls. Non-nil enables the one-shot warning about
	// monitors on a chain no unit polls.
	knownNetworks []string
	// warnedOrphans latches that warning so it cannot repeat.
	warnedOrphans bool
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
		Network:             p.network,
		LastProcessedLedger: processed,
		LatestChainLedger:   latest,
		LastSuccessfulPoll:  at.UTC(),
	})
}

// New wires a Poller. src is where events come from: NewRPCSource for a
// Stellar RPC node, or the SoroTrail source for upstream mode.
func New(src EventSource, st Store, reg *rules.Registry, d Dispatcher, interval time.Duration, log *slog.Logger) *Poller {
	return &Poller{
		source:   src,
		store:    st,
		registry: reg,
		dispatch: d,
		interval: interval,
		log:      log,
		sched:    NewScheduler(),
	}
}

// WithMetrics attaches Prometheus instrumentation to the poll loop, labelled
// with this poller's network when one was set first.
func (p *Poller) WithMetrics(m *metrics.Metrics) *Poller {
	p.metrics = forNetwork(m, p.network)
	return p
}

// WithNetwork restricts a Poller to one Stellar network: it watches only that
// network's monitors, advances only that network's checkpoint, and retracts
// only that network's alerts on a reorg. Not calling it leaves the poller in
// the pre-multi-network mode — every monitor, the single legacy ingest row —
// which is what an instance that configures one network through the legacy
// fields still gets.
func (p *Poller) WithNetwork(network string) *Poller {
	p.network = network
	// Every line this poller emits is about one chain, so an operator reading
	// a two-network log can tell them apart. Metrics are labelled the same
	// way, whichever of the two With-methods runs first.
	if p.log != nil && network != "" {
		p.log = p.log.With("network", network)
	}
	p.metrics = forNetwork(p.metrics, network)
	return p
}

// forNetwork returns m labelled for network, or m unchanged when there is no
// network to label with. metrics is nil-safe, so a nil m is expected here.
func forNetwork(m *metrics.Metrics, network string) *metrics.Metrics {
	if m == nil || network == "" {
		return m
	}
	return m.WithNetwork(network)
}

// Network reports the network this poller ingests, "" for the legacy
// all-networks mode. The supervisor and the health endpoint key their
// per-network reporting on it.
func (p *Poller) Network() string { return p.network }

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
			p.log.Error("poll failed", "err", err, "retry_in", delay)
			continue
		}
		delay = p.interval
	}
}

// Poll runs one ingest cycle. Exported so tests (and one-shot tools) can
// drive the poller without the timing loop.
func (p *Poller) Poll(ctx context.Context) error {
	monitors, err := p.store.ListMonitors(ctx, true)
	if err != nil {
		return err
	}
	// One poller per network, so every other network's monitors are not just
	// uninteresting here, they are wrong: the same contract id on two chains
	// names two different things. The filter happens after the read rather
	// than in it because ListMonitors is the shared, unfiltered listing the
	// rest of the ingest path uses; a monitor whose network is still empty
	// belongs to no chain yet, so no named poller claims it — startup's
	// AssignLegacyNetwork is what labels those rows.
	if p.network != "" {
		own := make([]store.Monitor, 0, len(monitors))
		var orphans []string
		polled := make(map[string]bool, len(p.knownNetworks))
		for _, n := range p.knownNetworks {
			polled[n] = true
		}
		for _, m := range monitors {
			if m.Network == p.network {
				own = append(own, m)
				continue
			}
			// knownNetworks is non-nil only on the supervisor's reporting
			// unit, so this neither runs per poller nor cries wolf about a
			// chain a sibling unit does poll. A monitor outside that set is
			// watched by nobody and will never alert.
			if p.knownNetworks != nil && !polled[m.Network] {
				label := m.Network
				if label == "" {
					// Rows startup's AssignLegacyNetwork missed: no chain at
					// all, which is the more common half of this mistake.
					label = "<none>"
				}
				orphans = append(orphans, fmt.Sprintf("%d=%s", m.ID, label))
			}
		}
		if len(orphans) > 0 && !p.warnedOrphans {
			// Warn once per process: the set rarely changes, and a line every
			// cycle would bury everything else the poller reports.
			p.warnedOrphans = true
			p.log.Warn("monitors belong to no polled network",
				"monitors", strings.Join(orphans, ","),
				"polling", strings.Join(p.knownNetworks, ","))
		}
		monitors = own
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

	state, err := p.store.GetIngestState(ctx, p.network)
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
		if err := p.store.SetIngestState(ctx, p.network, state); err != nil {
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
		if err := p.store.SetIngestState(ctx, p.network, state); err != nil {
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

// pollBatch pages through getEvents for one set of filters, following the
// cursor until the stream is drained. Returns the node's latestLedger.
// handleEvent runs every enabled rule of every monitor watching the
// event's contract. Events arrive already decoded from the source;
// per-event failures are logged, not fatal: one bad event must not stall
// ingestion.
func (p *Poller) handleEvent(ctx context.Context, decoded *stellar.DecodedEvent, byContract map[string][]store.Monitor) {
	monitors, watched := byContract[decoded.ContractID]
	if !watched {
		return
	}

	for _, m := range monitors {
		ruleList, err := p.store.ListRules(ctx, m.ID, true)
		if err != nil {
			p.log.Error("list rules", "monitor_id", m.ID, "err", err)
			continue
		}
		for _, rule := range ruleList {
			// The rule id rides in the context so a stateful evaluator (the
			// frequency rule) can key its per-rule state.
			ruleCtx := rules.WithRuleID(ctx, rule.ID)
			matched, err := p.registry.Evaluate(ruleCtx, rule.Type, decoded, rule.Params)
			if err != nil {
				p.log.Warn("rule evaluation failed", "rule_id", rule.ID, "event_id", decoded.ID, "err", err)
				continue
			}
			if matched {
				p.matched++
				p.metrics.RecordAlert()
				// An evaluator may override the dedup key (the frequency rule
				// stores every crossing in one episode under the window start).
				eventID := decoded.ID
				if id := p.registry.AlertEventID(ruleCtx, rule.Type, decoded, rule.Params); id != "" {
					eventID = id
				}
				p.fireAlert(ctx, m, rule, decoded, eventID)
			}
		}
	}
}

// fireAlert persists a deduped, cooldown-gated alert and hands it to the
// dispatcher. The store owns both gates so they hold across poller instances
// and restarts; this function only reports the outcome.
func (p *Poller) fireAlert(ctx context.Context, m store.Monitor, rule store.Rule, ev *stellar.DecodedEvent, eventID string) {
	body := map[string]any{
		"contract_id":      ev.ContractID,
		"event_name":       ev.EventName(),
		"ledger":           ev.Ledger,
		"ledger_closed_at": ev.LedgerClosedAt,
		"tx_hash":          ev.TxHash,
		"topics":           ev.Topics,
		"value":            ev.Value,
	}
	// Named fields are only present when the contract's spec was available;
	// without one the payload is byte-for-byte what it has always been.
	if ev.Fields != nil {
		body["fields"] = ev.Fields
	}
	payload, err := json.Marshal(body)
	if err != nil {
		p.log.Error("marshal alert payload", "event_id", ev.ID, "err", err)
		return
	}

	alert := &store.Alert{
		MonitorID:      m.ID,
		RuleID:         rule.ID,
		EventID:        eventID,
		Payload:        payload,
		Ledger:         ev.Ledger,
		LedgerClosedAt: ev.LedgerClosedAt,
		Cooldown:       ruleCooldown(rule),
	}
	outcome, err := p.store.CreateAlert(ctx, alert)
	if err != nil {
		p.log.Error("create alert", "rule_id", rule.ID, "event_id", ev.ID, "err", err)
		return
	}
	switch outcome {
	case store.AlertDuplicate:
		return // dedup: this rule already fired for this event
	case store.AlertSuppressed:
		// Counted by the store; logged so an operator can see the rule is
		// firing far more often than it is alerting.
		p.log.Info("alert suppressed by cooldown",
			"rule_id", rule.ID, "event_id", ev.ID, "cooldown", alert.Cooldown)
		return
	}
	logAttrs := []any{"alert_id", alert.ID, "monitor", m.Name, "rule_id", rule.ID, "event_id", ev.ID}
	if alert.SuppressedSinceLast > 0 {
		logAttrs = append(logAttrs, "suppressed_since_last", alert.SuppressedSinceLast)
	}
	p.log.Info("alert created", logAttrs...)

	p.dispatch.Dispatch(ctx, notify.Alert{
		ID:          alert.ID,
		MonitorID:   m.ID,
		MonitorName: m.Name,
		RuleID:      rule.ID,
		RuleType:    rule.Type,
		EventID:     eventID,
		ContractID:  ev.ContractID,
		EventName:   ev.EventName(),
		Ledger:      ev.Ledger,
		TxHash:      ev.TxHash,
		// The store folds the suppressed count into the payload, so the
		// notification reports it too.
		Payload:   alert.Payload,
		CreatedAt: alert.CreatedAt,
	})
}

// ruleCooldown reads a rule's optional cooldown. It is validated when the rule
// is created, so a value that no longer parses is treated as "no cooldown"
// rather than dropping matches.
func ruleCooldown(rule store.Rule) time.Duration {
	d, err := rules.ParseCooldown(rule.Params)
	if err != nil {
		return 0
	}
	return d
}
