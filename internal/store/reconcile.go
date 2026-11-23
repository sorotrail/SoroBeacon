package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// DefaultReconcileInterval is how often the background reconciliation runs.
const DefaultReconcileInterval = 5 * time.Minute

// RunChannelAckReconciler runs background delivery-vs-acknowledgement reconciliation
// periodically until ctx is cancelled.
func RunChannelAckReconciler(ctx context.Context, st Store, interval time.Duration, cfg AckReconciliationConfig, log *slog.Logger) {
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	sysCtx := workspace.WithSystem(ctx)
	reconcileOnce := func() {
		results, err := st.ReconcileChannelAcknowledgements(sysCtx, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("channel ack reconciliation failed", "err", err)
			return
		}
		var flaggedCount int
		for _, r := range results {
			if r.Flagged {
				flaggedCount++
			}
		}
		log.Info("channel ack reconciliation completed", "channels", len(results), "flagged", flaggedCount)
	}

	reconcileOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("channel ack reconciler stopped")
			return
		case <-t.C:
			reconcileOnce()
		}
	}
}
