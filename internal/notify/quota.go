package notify

import (
	"context"
	"fmt"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// The delivery budget is the per-monitor backstop behind channel rate
// limiting. Rate limiting paces a flood; it does not stop it, and the bill,
// the Slack channel and the on-call person all still lose. A budget caps how
// many alerts a monitor may deliver per UTC day and then stops.
//
// It is separate from MonitorBudgetTracker on purpose. That tracker is an
// in-memory, per-process volume cap over a short rolling window: it forgets on
// restart and it still lets a summary through. This budget is durable (a
// restart must not reopen the floodgate), daily, opt-in per monitor and
// unlimited by default, so a deployment that never sets one is unaffected.
//
// What exhaustion does is stop delivering rather than fall back to a digest:
// "stopped, and it said so once" is the clearer contract. Alerts are still
// stored and marked suppressed with the reason, so nothing is lost, only not
// sent. The budget refills at UTC midnight or when an operator resets it.

// WithDeliveryBudgets attaches the durable per-monitor budget store. Without
// it every monitor is unlimited.
func (d *Dispatcher) WithDeliveryBudgets(b store.DeliveryBudgets) *Dispatcher {
	d.budgets = b
	return d
}

// checkDeliveryBudget spends one unit of the alert's monitor budget and
// reports whether normal delivery should proceed.
//
// It fails open: if the budget cannot be read, the alert is delivered. A
// broken budget store must not turn a safety net into an outage of alerting.
func (d *Dispatcher) checkDeliveryBudget(ctx context.Context, a Alert) bool {
	if d.budgets == nil || a.MonitorID <= 0 {
		return true
	}
	dec, err := d.budgets.ConsumeDelivery(ctx, a.MonitorID, d.nowTime())
	if err != nil {
		d.log.Error("consume delivery budget", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		return true
	}
	if dec.Allowed {
		return true
	}

	reason := fmt.Sprintf("monitor %d delivery budget exhausted (%d of %d used, resets %s)",
		a.MonitorID, dec.Used, dec.Limit, dec.ResetsAt.Format("2006-01-02 15:04 MST"))
	if err := d.store.SetAlertSuppressed(ctx, a.ID, reason); err != nil {
		d.log.Error("mark alert suppressed due to delivery budget", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
	}
	if d.metrics != nil {
		d.metrics.RecordThrottle("delivery_budget")
	}
	if !dec.Announce {
		d.log.Debug("delivery budget exhausted; alert stored not delivered", "alert_id", a.ID, "monitor_id", a.MonitorID)
		return false
	}

	d.log.Warn("monitor delivery budget exhausted; delivering notice and stopping",
		"alert_id", a.ID, "monitor_id", a.MonitorID, "limit", dec.Limit)
	if d.metrics != nil {
		d.metrics.RecordDeliveryBudgetExhausted()
	}
	d.announceBudgetExhausted(ctx, a, dec)
	return false
}

// announceBudgetExhausted tells the monitor's channels, once per exhaustion,
// that delivery has stopped. A budget that silently stops alerting is worse
// than the flood it prevents, so the notice goes straight to every enabled
// channel: it is critical so a channel's minimum severity cannot hide it, and
// it skips digest windows and escalation so it is not delayed or paged on.
// The text carries only counts and a time, never channel configuration.
func (d *Dispatcher) announceBudgetExhausted(ctx context.Context, a Alert, dec store.BudgetDecision) {
	channels, err := d.store.ListChannelsForMonitor(ctx, a.MonitorID)
	if err != nil {
		d.log.Error("list channels for budget notice", "alert_id", a.ID, "monitor_id", a.MonitorID, "err", err)
		return
	}
	notice := a
	notice.Severity = "critical"
	notice.Digest = fmt.Sprintf(
		"Delivery budget exhausted for monitor %q: %d alerts delivered today (limit %d). "+
			"Further alerts are still recorded but will not be delivered until the budget resets at %s or is reset manually.",
		a.MonitorName, dec.Used, dec.Limit, dec.ResetsAt.Format("2006-01-02 15:04 MST"))
	d.deliverChannelGroup(ctx, notice, channels)
}
