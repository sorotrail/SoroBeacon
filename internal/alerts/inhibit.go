// Package alerts holds delivery-time alert decisions that apply uniformly
// regardless of rule type: grouping, and now inhibition. It is kept out of
// internal/rules (which owns matching) and internal/notify (which owns
// transport) so the decision logic stays unit-testable without a database.
package alerts

import (
	"context"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// InhibitedBy evaluates whether deliveries for targetRuleID must be
// suppressed. inhibitions carries the pairs targeting that rule; firedWithin
// answers the firing question per source rule (backed by
// Store.RuleFiredWithin in production, faked in tests).
//
// Evaluation is deliberately single-hop: only the direct sources of the
// target are consulted, never the sources of the sources. A cycle such as A
// inhibits B inhibits A therefore terminates by construction — there is no
// recursion to guard and no depth limit to tune. The first firing source
// wins; its id is returned so the caller can record why delivery stopped.
//
// A nil firedWithin (or no inhibitions) means "nothing is firing": deliver.
func InhibitedBy(
	ctx context.Context,
	targetRuleID int64,
	inhibitions []store.Inhibition,
	firedWithin func(ctx context.Context, ruleID int64, window time.Duration) (bool, error),
) (sourceRuleID int64, inhibited bool, err error) {
	if len(inhibitions) == 0 || firedWithin == nil {
		return 0, false, nil
	}
	for _, in := range inhibitions {
		if in.TargetRuleID != targetRuleID {
			continue
		}
		window := time.Duration(in.FiringWindowSeconds) * time.Second
		if window <= 0 {
			window = time.Duration(store.DefaultInhibitionWindowSeconds) * time.Second
		}
		firing, err := firedWithin(ctx, in.SourceRuleID, window)
		if err != nil {
			return 0, false, err
		}
		if firing {
			return in.SourceRuleID, true, nil
		}
	}
	return 0, false, nil
}
