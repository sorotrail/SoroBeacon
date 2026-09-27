// Package backfill replays a bounded stretch of a source's ledger history for
// one monitor, evaluating that monitor's rules against the events and
// recording the matches. It exists because a newly created monitor otherwise
// sees only events from the moment it was created: anyone adding a monitor for
// a contract that has been live for months gets an empty dashboard and no way
// to test rules against real data.
//
// Backfill is opt-in. Nothing here runs unless a caller (the `sorobeacon
// backfill` subcommand) asks for it, so creating a monitor never triggers a
// replay. It also reads through the same poller.EventSource the live poller
// uses rather than the RPC directly, so upstream (SoroTrail) mode works too and
// the source's own batching, filters and decoding apply.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// DefaultRate is the minimum delay between page fetches when Options.Rate is
// unset. The backfill and the live poller share one source, so pacing the
// replay keeps it from starving live polling.
const DefaultRate = 200 * time.Millisecond

// ledgerCloseInterval approximates Stellar's ledger cadence, used only to turn
// a lookback duration into a start ledger. The source's retention clamp and the
// reported Result keep the estimate honest.
const ledgerCloseInterval = 5 * time.Second

// Options configures one backfill run. Either FromLedger or Lookback must be
// set; ToLedger defaults to the source's current tip.
type Options struct {
	// MonitorID is the monitor whose contracts and rules are replayed.
	MonitorID int64
	// FromLedger is the inclusive first ledger to replay. When set it takes
	// precedence over Lookback.
	FromLedger uint32
	// ToLedger is the inclusive last ledger to replay. Zero means the source's
	// current tip.
	ToLedger uint32
	// Lookback resolves FromLedger as this long before ToLedger, using the
	// ~5s ledger cadence. Ignored when FromLedger is set.
	Lookback time.Duration
	// Deliver hands backfilled alerts to the monitor's channels. It defaults
	// to false: replaying months of history must not page anyone.
	Deliver bool
	// Rate is the minimum delay between page fetches. Zero means DefaultRate.
	Rate time.Duration
	// PageSize is the getEvents page size. Zero uses the source's default.
	PageSize int
}

// validate rejects an unusable range before any source or store work happens.
func (o Options) validate() error {
	if o.MonitorID == 0 {
		return errors.New("backfill: monitor id is required")
	}
	if o.FromLedger == 0 && o.Lookback <= 0 {
		return errors.New("backfill: from_ledger or lookback is required")
	}
	return nil
}

// Result reports what a run did, including how far back it could actually go.
type Result struct {
	MonitorID int64
	// RequestedFrom is the start the caller asked for, before clamping.
	RequestedFrom uint32
	// FromLedger is the start the run actually used.
	FromLedger uint32
	ToLedger   uint32
	// OldestLedger is the oldest ledger the source still holds, or 0 when the
	// source does not report it.
	OldestLedger uint32
	// Clamped is true when RequestedFrom predated the source's retention and
	// the run started later than asked.
	Clamped bool
	// Resumed is true when the run continued an interrupted backfill instead
	// of starting from the beginning.
	Resumed bool
	// Ledgers is the number of ledgers the run's range covers.
	Ledgers uint32
	// Events, Matched, Alerts and Dispatched are the run's totals.
	Events     int
	Matched    int
	Alerts     int
	Dispatched int
}

// Store is the slice of persistence the job needs. poller.IngestStore (used by
// the shared Ingestor) is a subset of it, so one store backs both.
type Store interface {
	GetMonitor(ctx context.Context, id int64) (*store.Monitor, error)
	ListRules(ctx context.Context, monitorID int64, enabledOnly bool) ([]store.Rule, error)
	GetBackfill(ctx context.Context, monitorID int64) (store.Backfill, error)
	UpsertBackfill(ctx context.Context, b *store.Backfill) error
}

// Job replays a bounded ledger range for one monitor through the rules engine.
// It is constructed once and can run more than one job.
type Job struct {
	source   poller.EventSource
	store    Store
	registry *rules.Registry
	ingestor *poller.Ingestor
	log      *slog.Logger
}

// New wires a Job. source is the same event source the live poller uses.
func New(src poller.EventSource, st Store, reg *rules.Registry, ing *poller.Ingestor, log *slog.Logger) *Job {
	return &Job{source: src, store: st, registry: reg, ingestor: ing, log: log}
}

// Run replays the configured range. It persists progress after every page, so
// a run interrupted by a crash or an error resumes from the last page when it
// is next invoked. Returns when the range is drained or ctx is cancelled.
func (j *Job) Run(ctx context.Context, opts Options) (Result, error) {
	if err := opts.validate(); err != nil {
		return Result{}, err
	}

	m, err := j.store.GetMonitor(ctx, opts.MonitorID)
	if err != nil {
		return Result{}, fmt.Errorf("backfill: load monitor %d: %w", opts.MonitorID, err)
	}
	ruleList, err := j.store.ListRules(ctx, m.ID, true)
	if err != nil {
		return Result{}, fmt.Errorf("backfill: list rules for monitor %d: %w", m.ID, err)
	}
	if len(ruleList) == 0 {
		j.log.Warn("backfill: monitor has no enabled rules; nothing will match", "monitor_id", m.ID)
	}

	watch := poller.WatchesFor(j.registry, ruleList, m.ContractIDs)
	if len(watch) == 0 {
		return Result{}, fmt.Errorf("backfill: monitor %d watches no valid contracts", m.ID)
	}

	tip, err := j.source.LatestLedger(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("backfill: read source tip: %w", err)
	}
	to := opts.ToLedger
	if to == 0 || to > tip {
		to = tip
	}

	from := opts.FromLedger
	if from == 0 {
		from = lookbackStart(to, opts.Lookback)
	}
	if from > to {
		from = to
	}

	res := Result{MonitorID: m.ID, RequestedFrom: from, ToLedger: to}

	// Clamp to what the source can still serve, and say so: silently returning
	// less than asked is worse than a clear "this is as far back as I could go".
	if reporter, ok := j.source.(poller.RetentionReporter); ok {
		oldest, err := reporter.OldestLedger(ctx)
		switch {
		case err != nil:
			j.log.Warn("backfill: could not read source retention; using the requested range", "err", err)
		case oldest > 0:
			res.OldestLedger = oldest
			if oldest > from {
				j.log.Warn("backfill: requested range predates source retention; starting at the oldest retained ledger",
					"monitor_id", m.ID, "requested_from", from, "oldest_ledger", oldest)
				res.Clamped = true
				from = oldest
			}
		}
	}
	if from > to {
		// The whole requested window is older than the source can serve.
		from = to
	}
	res.FromLedger = from

	// Resume an interrupted run rather than restarting: the source cursor
	// continues mid-page, and NextLedger restarts the range if the cursor is
	// empty. Dedup on (rule_id, event_id) makes a replay of already-recorded
	// pages harmless, so resuming is always safe.
	start, cursor := from, ""
	prev, err := j.store.GetBackfill(ctx, m.ID)
	switch {
	case err == nil && !prev.Complete:
		start, cursor = prev.NextLedger, prev.Cursor
		res.Resumed = true
		j.log.Info("backfill: resuming interrupted run",
			"monitor_id", m.ID, "next_ledger", start, "cursor_set", cursor != "")
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return res, fmt.Errorf("backfill: load progress for monitor %d: %w", m.ID, err)
	}

	prog := store.Backfill{
		MonitorID:  m.ID,
		FromLedger: res.FromLedger,
		ToLedger:   to,
		NextLedger: start,
		Cursor:     cursor,
		Deliver:    opts.Deliver,
	}
	if err := j.store.UpsertBackfill(ctx, &prog); err != nil {
		return res, fmt.Errorf("backfill: save progress: %w", err)
	}

	rate := opts.Rate
	if rate <= 0 {
		rate = DefaultRate
	}
	watched := contractSet(m.ContractIDs)
	res.Ledgers = to - res.FromLedger + 1

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		page, err := j.source.FetchEvents(ctx, start, watch, cursor, opts.PageSize)
		if err != nil {
			// Persist the resume point before returning so the next run
			// continues from this page instead of replaying the whole range.
			prog.NextLedger, prog.Cursor = start, cursor
			if saveErr := j.store.UpsertBackfill(ctx, &prog); saveErr != nil {
				j.log.Error("backfill: save progress after fetch error", "monitor_id", m.ID, "err", saveErr)
			}
			return res, fmt.Errorf("backfill: fetch events: %w", err)
		}

		reachedEnd := false
		for _, ev := range page.Events {
			// The source returns events ascending by ledger and the job asked
			// for a bounded range, so once an event passes ToLedger the range
			// is drained: stop rather than replay the present.
			if ev.Ledger > to {
				reachedEnd = true
				continue
			}
			if !watched[ev.ContractID] {
				continue
			}
			r := j.ingestor.Handle(ctx, ev, []store.Monitor{*m}, poller.HandleOptions{
				Backfilled: true,
				Deliver:    opts.Deliver,
			})
			res.Events++
			res.Matched += r.Matched
			res.Alerts += r.Alerts
			res.Dispatched += r.Dispatched
		}

		if reachedEnd || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		prog.NextLedger, prog.Cursor = start, cursor
		if err := j.store.UpsertBackfill(ctx, &prog); err != nil {
			return res, fmt.Errorf("backfill: save progress: %w", err)
		}
		if err := sleep(ctx, rate); err != nil {
			return res, err
		}
	}

	prog.Complete = true
	prog.Cursor = ""
	if err := j.store.UpsertBackfill(ctx, &prog); err != nil {
		return res, fmt.Errorf("backfill: save completion: %w", err)
	}
	j.log.Info("backfill complete",
		"monitor_id", m.ID,
		"from_ledger", res.FromLedger,
		"to_ledger", to,
		"clamped", res.Clamped,
		"resumed", res.Resumed,
		"events", res.Events,
		"matched", res.Matched,
		"alerts", res.Alerts,
		"dispatched", res.Dispatched,
	)
	return res, nil
}

// lookbackStart turns a lookback duration into an inclusive start ledger
// before end. It is an estimate (the chain's true ledger cadence varies) and
// deliberately never reaches below ledger 1.
func lookbackStart(end uint32, lookback time.Duration) uint32 {
	if lookback <= 0 {
		return end
	}
	n := uint64(lookback / ledgerCloseInterval)
	if n >= uint64(end) {
		return 1
	}
	return end - uint32(n)
}

// contractSet indexes the monitor's contracts so events from anything else the
// source happens to return are ignored.
func contractSet(contracts []string) map[string]bool {
	set := make(map[string]bool, len(contracts))
	for _, c := range contracts {
		set[c] = true
	}
	return set
}

// sleep waits for d, or returns ctx.Err() if the context ends first.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
