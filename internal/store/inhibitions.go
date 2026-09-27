package store

import (
	"context"
	"time"
)

// DefaultInhibitionWindowSeconds is the firing window applied when an
// Inhibition is created without an explicit one: long enough to keep a
// sustained incident quiet, short enough that a new episode the next day
// is not hidden behind an old failure.
const DefaultInhibitionWindowSeconds = 300

// Inhibition pairs two rules: while the source rule has produced an alert
// within FiringWindowSeconds, deliveries for the target rule are suppressed
// (the alert rows themselves are always kept). Cross-monitor pairs are
// allowed: one incident usually trips several monitors at once, and the
// root-cause page is what the operator wants.
type Inhibition struct {
	SourceRuleID        int64     `json:"source_rule_id"`
	TargetRuleID        int64     `json:"target_rule_id"`
	FiringWindowSeconds int       `json:"firing_window_seconds"`
	CreatedAt           time.Time `json:"created_at"`
}

// Inhibitions persists inhibition pairs plus the two signals the
// delivery-time decision needs: whether a rule fired recently, and the mark
// recording which source suppressed a delivery.
type Inhibitions interface {
	// CreateInhibition stores a pair. A non-positive FiringWindowSeconds is
	// replaced with DefaultInhibitionWindowSeconds, and CreatedAt is filled
	// in from the stored row.
	CreateInhibition(ctx context.Context, in *Inhibition) error
	ListInhibitions(ctx context.Context) ([]Inhibition, error)
	// ListInhibitionsForTarget returns the pairs suppressing one rule.
	ListInhibitionsForTarget(ctx context.Context, targetRuleID int64) ([]Inhibition, error)
	// DeleteInhibition removes one pair; ErrNotFound when it is absent.
	DeleteInhibition(ctx context.Context, sourceRuleID, targetRuleID int64) error
	// RuleFiredWithin reports whether the rule produced an alert during the
	// trailing window.
	RuleFiredWithin(ctx context.Context, ruleID int64, window time.Duration) (bool, error)
	// MarkAlertInhibited records which source rule suppressed an alert's
	// delivery. Missing rows are not an error: the alert may have been
	// pruned between dispatch and this write.
	MarkAlertInhibited(ctx context.Context, alertID, sourceRuleID int64) error
}
