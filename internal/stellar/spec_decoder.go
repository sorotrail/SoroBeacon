package stellar

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Defaults for SpecDecoder. Specifications rarely change for a deployed
// contract, but a contract can be upgraded in place, so the positive cache
// still expires. Negative results expire sooner so a contract that later gains
// a spec is picked up without a restart, while a contract with no spec is not
// re-fetched on every event.
const (
	DefaultSpecTTL          = time.Hour
	DefaultSpecNegativeTTL  = time.Minute
	DefaultSpecFetchTimeout = 5 * time.Second
)

// SpecDecoder wraps another Decoder and, when a contract's spec can be
// fetched, fills DecodedEvent.Fields with the spec's named event parameters.
// It is strictly additive: the wrapped decoder's Topics and Value are left
// exactly as produced, and any contract without a usable spec decodes
// identically to the wrapped decoder alone.
//
// Specs are fetched lazily on first sight of a contract and cached (including
// negative results), so a busy contract pays for at most one fetch per TTL.
// A fetch failure is logged, cached briefly, and degrades to the wrapped
// decoder's output rather than failing or delaying the event past the fetch
// timeout.
type SpecDecoder struct {
	base  Decoder
	specs SpecSource
	log   *slog.Logger

	positiveTTL  time.Duration
	negativeTTL  time.Duration
	fetchTimeout time.Duration
	now          func() time.Time

	mu    sync.Mutex
	cache map[string]specCacheEntry
}

type specCacheEntry struct {
	spec    *ContractSpec
	expires time.Time
}

// NewSpecDecoder wraps base with spec-aware named-field decoding. A nil base
// falls back to DefaultDecoder and a nil logger to slog.Default.
func NewSpecDecoder(base Decoder, specs SpecSource, log *slog.Logger) *SpecDecoder {
	if base == nil {
		base = DefaultDecoder{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &SpecDecoder{
		base:         base,
		specs:        specs,
		log:          log,
		positiveTTL:  DefaultSpecTTL,
		negativeTTL:  DefaultSpecNegativeTTL,
		fetchTimeout: DefaultSpecFetchTimeout,
		now:          time.Now,
		cache:        make(map[string]specCacheEntry),
	}
}

// WithTTL overrides how long positive and negative spec results are cached.
func (d *SpecDecoder) WithTTL(positive, negative time.Duration) *SpecDecoder {
	d.positiveTTL = positive
	d.negativeTTL = negative
	return d
}

// WithFetchTimeout bounds a single spec fetch; the event falls back to the
// default decoding if the fetch outlives it.
func (d *SpecDecoder) WithFetchTimeout(timeout time.Duration) *SpecDecoder {
	d.fetchTimeout = timeout
	return d
}

var _ Decoder = (*SpecDecoder)(nil)

// DecodeEvent decodes with the wrapped decoder first, then augments the result
// with named fields when a spec is available. Spec lookup never changes the
// decoded Topics or Value, so a rule written against positional topics keeps
// working unchanged.
func (d *SpecDecoder) DecodeEvent(ctx context.Context, ev Event) (*DecodedEvent, error) {
	out, err := d.base.DecodeEvent(ctx, ev)
	if err != nil {
		return nil, err
	}
	if spec := d.contractSpec(ctx, ev.ContractID); spec != nil {
		// A shape mismatch just means the event is not one the spec describes;
		// the positional decoding already in out stands.
		spec.FillFields(out)
	}
	return out, nil
}

// contractSpec returns the cached spec for contractID, fetching it if the
// cache entry has expired. It returns nil when the contract has no spec or the
// fetch failed; both are cached so the next event does not retry immediately.
func (d *SpecDecoder) contractSpec(ctx context.Context, contractID string) *ContractSpec {
	if contractID == "" || d.specs == nil {
		return nil
	}

	d.mu.Lock()
	if entry, ok := d.cache[contractID]; ok && d.now().Before(entry.expires) {
		d.mu.Unlock()
		return entry.spec
	}
	d.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, d.fetchTimeout)
	defer cancel()
	spec, err := d.specs.ContractSpec(fetchCtx, contractID)

	d.mu.Lock()
	defer d.mu.Unlock()
	entry := specCacheEntry{spec: spec, expires: d.now().Add(d.positiveTTL)}
	switch {
	case err == nil && spec != nil:
		d.cache[contractID] = entry
		return spec
	case errors.Is(err, ErrNoSpec) || (err == nil && spec == nil):
		d.cache[contractID] = specCacheEntry{expires: d.now().Add(d.negativeTTL)}
		return nil
	default:
		d.cache[contractID] = specCacheEntry{expires: d.now().Add(d.negativeTTL)}
		d.log.Warn("contract spec fetch failed; using default decoding",
			"contract_id", contractID, "err", err)
		return nil
	}
}
