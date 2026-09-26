package poller

import (
	"context"
	"errors"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// ErrLedgerHashesUnsupported is returned by a source whose backend cannot
// report ledger hashes (an upstream indexer, or an RPC node too old for
// getLedgers). The poller treats it as "reorg detection not available" and
// carries on rather than failing the cycle.
var ErrLedgerHashesUnsupported = errors.New("ledger hashes not supported by this event source")

// LedgerHashSource is the optional EventSource capability reorg detection
// needs: the hash of every ledger in an inclusive range. A source that cannot
// supply hashes does not have to implement this interface at all.
//
// Design note: SoroBeacon detects a reorg by re-reading the hash of a ledger
// it already recorded and finding it changed. The alternative the issue
// mentions — walking each ledger's parent hash — was rejected because
// getEvents exposes no ledger identity and the header that carries the parent
// link is base64 XDR, so it would add an XDR decode on the hot path for no
// extra detection power once hashes are tracked.
type LedgerHashSource interface {
	LedgerHashes(ctx context.Context, from, to uint32) ([]store.LedgerHash, error)
}

// detectReorg re-reads the recent ledger-hash window and, if any recorded
// ledger's hash changed, retracts the alerts derived from the orphaned range
// and returns the divergence ledger (0 when the chain is intact). It records
// the observed hashes and prunes the window either way.
//
// It is safe to call when reorg detection is disabled or the source cannot
// report hashes: both are a no-op.
func (p *Poller) detectReorg(ctx context.Context) (uint32, error) {
	if p.reorgWindow == 0 {
		return 0, nil
	}
	src, ok := p.source.(LedgerHashSource)
	if !ok {
		return 0, nil
	}

	tip, err := p.source.LatestLedger(ctx)
	if err != nil {
		return 0, err
	}
	if tip == 0 {
		return 0, nil
	}
	from := uint32(1)
	if tip > p.reorgWindow {
		from = tip - p.reorgWindow + 1
	}

	observed, err := src.LedgerHashes(ctx, from, tip)
	if err != nil {
		if errors.Is(err, ErrLedgerHashesUnsupported) {
			return 0, nil
		}
		return 0, err
	}
	if len(observed) == 0 {
		return 0, nil
	}

	stored, err := p.store.LedgerHashes(ctx, from, tip)
	if err != nil {
		return 0, err
	}
	known := make(map[uint32]string, len(stored))
	for _, h := range stored {
		known[h.Ledger] = h.Hash
	}

	// The divergence is the earliest ledger whose hash no longer matches
	// what was ingested; every alert at or after it came from a chain that is
	// no longer canonical.
	var divergence uint32
	for _, h := range observed {
		if old, seen := known[h.Ledger]; seen && old != h.Hash {
			if divergence == 0 || h.Ledger < divergence {
				divergence = h.Ledger
			}
		}
	}

	if divergence != 0 {
		retracted, err := p.store.RetractAlertsFromLedger(ctx, divergence, time.Now().UTC())
		if err != nil {
			return 0, err
		}
		// A delivered alert cannot be unsent, so the correction is a marker,
		// not a notification: it stops the API and dashboard presenting the
		// alert as canonical without risking an alert storm from a correction
		// loop.
		p.metrics.RecordReorg(divergence)
		p.log.Warn("ledger reorganisation detected",
			"from_ledger", divergence,
			"tip", tip,
			"window", p.reorgWindow,
			"retracted_alerts", retracted)
	}

	if err := p.store.RecordLedgerHashes(ctx, observed); err != nil {
		return 0, err
	}
	if from > 1 {
		if err := p.store.PruneLedgerHashes(ctx, from); err != nil {
			return 0, err
		}
	}
	return divergence, nil
}
