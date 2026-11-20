package store

import (
	"context"
	"errors"
	"time"
)

// ErrInvalidDeliveryBudget reports a budget limit the store will not hold.
// Zero would mean "deliver nothing", which is what disabling the monitor is
// for, so a limit must be positive; clearing the budget is a separate call.
var ErrInvalidDeliveryBudget = errors.New("delivery budget limit must be positive")

// DeliveryBudget is one monitor's delivery budget and how much of the current
// window is spent. A nil Limit means unlimited, which is the default for every
// monitor, so a deployment that never sets a budget behaves exactly as before.
//
// The window is the UTC calendar day: it is easy to reason about, it needs no
// per-monitor clock, and "resets at 00:00 UTC" is something an on-call person
// can be told in one line.
type DeliveryBudget struct {
	MonitorID int64 `json:"monitor_id"`
	// Limit is the number of alert deliveries allowed per window; nil is
	// unlimited.
	Limit *int `json:"limit"`
	// Used is how many deliveries the current window has consumed.
	Used int `json:"used"`
	// WindowStart is the UTC midnight the counter belongs to.
	WindowStart time.Time `json:"window_start"`
	// ExhaustedAt is when the budget ran out in this window, nil while there
	// is budget left. It is what makes the exhaustion notice go out once.
	ExhaustedAt *time.Time `json:"exhausted_at"`
}

// Exhausted reports whether the budget is spent for the current window.
func (b DeliveryBudget) Exhausted() bool { return b.ExhaustedAt != nil }

// Remaining is how many deliveries are left in the window, or -1 for an
// unlimited budget.
func (b DeliveryBudget) Remaining() int {
	if b.Limit == nil {
		return -1
	}
	if r := *b.Limit - b.Used; r > 0 {
		return r
	}
	return 0
}

// ResetsAt is when the window rolls over and the budget refills.
func (b DeliveryBudget) ResetsAt() time.Time { return b.WindowStart.Add(24 * time.Hour) }

// BudgetDecision is the answer to "may this alert be delivered".
type BudgetDecision struct {
	// Allowed is true when the delivery fits the budget. An unlimited monitor
	// is always allowed.
	Allowed bool
	// Announce is true for exactly one denied call per exhaustion: the one
	// that found the budget newly spent. The caller owes the monitor's
	// channels a notice then, and only then.
	Announce bool
	Limit    int
	Used     int
	ResetsAt time.Time
}

// DeliveryBudgets persists per-monitor delivery budgets and their consumption.
// It is keyed by monitor id alone, which is unique across workspaces, so the
// dispatcher — which runs without a request's workspace — can use it; callers
// that act for a user (the API, the dashboard) check the monitor in the
// caller's workspace first.
type DeliveryBudgets interface {
	// GetDeliveryBudget returns the monitor's budget as of now, with a stale
	// window already rolled over. ErrNotFound when the monitor does not exist.
	GetDeliveryBudget(ctx context.Context, monitorID int64, now time.Time) (*DeliveryBudget, error)
	// SetDeliveryBudget sets the daily limit, or clears it with nil so the
	// monitor is unlimited again. Raising the limit above what is already used
	// un-exhausts the budget. ErrInvalidDeliveryBudget for a non-positive
	// limit; ErrNotFound for an unknown monitor.
	SetDeliveryBudget(ctx context.Context, monitorID int64, limit *int) error
	// ConsumeDelivery atomically accounts one delivery against the budget and
	// reports whether it may go out. Two concurrent callers can never both
	// take the last unit.
	ConsumeDelivery(ctx context.Context, monitorID int64, now time.Time) (BudgetDecision, error)
	// ResetDeliveryBudget is the manual un-exhaust: it zeroes the window's
	// counter. ErrNotFound for an unknown monitor.
	ResetDeliveryBudget(ctx context.Context, monitorID int64, now time.Time) error
}

// budgetWindowStart is the UTC midnight at or before t.
func budgetWindowStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// rollBudget moves a stale window forward. A budget never used, or last used
// on an earlier day, starts today with nothing spent and no exhaustion.
func rollBudget(b DeliveryBudget, now time.Time) DeliveryBudget {
	start := budgetWindowStart(now)
	if b.WindowStart.IsZero() || b.WindowStart.Before(start) {
		b.WindowStart = start
		b.Used = 0
		b.ExhaustedAt = nil
	}
	return b
}

// decideDelivery is the budget rule, kept free of I/O so both backends share
// it and it can be tested without a database. It returns the budget to
// persist and the decision.
func decideDelivery(b DeliveryBudget, now time.Time) (DeliveryBudget, BudgetDecision) {
	if b.Limit == nil {
		return b, BudgetDecision{Allowed: true}
	}
	b = rollBudget(b, now)
	d := BudgetDecision{Limit: *b.Limit, ResetsAt: b.ResetsAt()}
	if b.Used < *b.Limit {
		b.Used++
		d.Allowed, d.Used = true, b.Used
		return b, d
	}
	d.Used = b.Used
	if b.ExhaustedAt == nil {
		at := now.UTC()
		b.ExhaustedAt = &at
		d.Announce = true
	}
	return b, d
}
