package notify

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// DefaultEscalationInterval is how often the scheduler looks for due
// escalation steps: small enough that a five-minute step is seconds late at
// worst, large enough not to poll the store in a tight loop.
const DefaultEscalationInterval = 10 * time.Second

// DefaultEscalationBatch bounds how many due escalations one pass processes,
// so a backlog cannot hold the scheduler for an unbounded time.
const DefaultEscalationBatch = 100

// startEscalation fires the first step now and persists the schedule for the
// rest. The first step's delay is relative to the alert, so it is due
// immediately; later steps are stored as an absolute next-due time, which is
// what lets a restart resume them instead of restarting or dropping them.
func (d *Dispatcher) startEscalation(ctx context.Context, a Alert, policy *store.EscalationPolicy) {
	d.deliverStep(ctx, a, policy.Steps[0])
	if len(policy.Steps) == 1 {
		return
	}
	// Snapshot the notification so a later step delivered after a restart can
	// be sent without rebuilding it from the alert payload.
	snapshot, err := json.Marshal(a)
	if err != nil {
		d.log.Error("marshal escalation snapshot", "alert_id", a.ID, "err", err)
		return
	}
	due := time.Now().Add(seconds(policy.Steps[1].DelaySeconds))
	if err := d.store.ScheduleEscalation(ctx, a.ID, policy.ID, snapshot, 1, due); err != nil {
		d.log.Error("schedule escalation", "alert_id", a.ID, "policy_id", policy.ID, "err", err)
	}
}

// deliverStep sends the alert to a step's channels. A channel disabled after
// the policy was written is skipped rather than erroring, matching the flat
// fan-out's ListChannelsForMonitor behaviour.
func (d *Dispatcher) deliverStep(ctx context.Context, a Alert, step store.EscalationStep) {
	if len(step.ChannelIDs) == 0 {
		return
	}
	channels, err := d.store.ListChannelsByIDs(ctx, step.ChannelIDs)
	if err != nil {
		d.log.Error("list escalation channels", "alert_id", a.ID, "err", err)
		return
	}
	for _, ch := range channels {
		d.deliver(ctx, a, ch)
	}
}

// ProcessDueEscalations fires every escalation step due at or before now and
// advances (or completes) each one. It is exported so the scheduler can be
// driven deterministically in tests instead of sleeping on a ticker.
func (d *Dispatcher) ProcessDueEscalations(ctx context.Context, now time.Time) (int, error) {
	batch := d.EscalationBatch
	if batch <= 0 {
		batch = DefaultEscalationBatch
	}
	runs, err := d.store.DueEscalations(ctx, now, batch)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, run := range runs {
		if ctx.Err() != nil {
			return delivered, ctx.Err()
		}
		var a Alert
		if err := json.Unmarshal(run.Snapshot, &a); err != nil {
			d.log.Error("decode escalation snapshot", "alert_id", run.AlertID, "err", err)
			_ = d.store.CompleteEscalation(ctx, run.AlertID)
			continue
		}
		policy, err := d.store.GetEscalationPolicy(ctx, run.PolicyID)
		if err != nil {
			// The policy was deleted mid-escalation, so there is nothing left
			// to fire; finish it rather than retrying this row forever.
			d.log.Warn("escalation policy missing; completing escalation",
				"alert_id", run.AlertID, "policy_id", run.PolicyID, "err", err)
			_ = d.store.CompleteEscalation(ctx, run.AlertID)
			continue
		}
		if run.NextStep < 0 || run.NextStep >= len(policy.Steps) {
			_ = d.store.CompleteEscalation(ctx, run.AlertID)
			continue
		}
		d.deliverStep(ctx, a, policy.Steps[run.NextStep])
		delivered++
		if run.NextStep+1 < len(policy.Steps) {
			nextDue := now.Add(seconds(policy.Steps[run.NextStep+1].DelaySeconds))
			if err := d.store.AdvanceEscalation(ctx, run.AlertID, run.NextStep+1, nextDue); err != nil {
				d.log.Error("advance escalation", "alert_id", run.AlertID, "err", err)
			}
		} else if err := d.store.CompleteEscalation(ctx, run.AlertID); err != nil {
			d.log.Error("complete escalation", "alert_id", run.AlertID, "err", err)
		}
	}
	return delivered, nil
}

// RunEscalations drives the scheduler until ctx is cancelled. Because due
// times are persisted, a process restart resumes the escalations in flight.
func (d *Dispatcher) RunEscalations(ctx context.Context) {
	interval := d.EscalationInterval
	if interval <= 0 {
		interval = DefaultEscalationInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	d.log.Info("escalation scheduler started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			d.log.Info("escalation scheduler stopped")
			return
		case <-t.C:
			n, err := d.ProcessDueEscalations(ctx, time.Now())
			if err != nil {
				if ctx.Err() != nil {
					continue
				}
				d.log.Error("process due escalations", "err", err)
				continue
			}
			if n > 0 {
				d.log.Info("escalation steps fired", "count", n)
			}
		}
	}
}

// seconds converts a stored delay to a duration, clamping a hand-edited value
// so a huge number cannot overflow into a negative (immediately-due) delay.
func seconds(n int64) time.Duration {
	const maxSeconds = (1<<63 - 1) / int64(time.Second)
	if n <= 0 {
		return 0
	}
	if n > maxSeconds {
		n = maxSeconds
	}
	return time.Duration(n) * time.Second
}
