package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const maxEnrichmentBytes int64 = 64 << 10

const (
	// DefaultEnrichmentCacheEntries bounds the cache. One entry per contract:
	// a thousand contracts is far beyond a typical watch list, and the bound
	// is what stops enrichment from becoming a memory leak.
	DefaultEnrichmentCacheEntries = 1024
	// DefaultEnrichmentStaleFor is how long past its TTL an entry may still be
	// served, labelled stale, when the source cannot be reached. After that the
	// alert goes out unenriched rather than with data nobody has confirmed.
	DefaultEnrichmentStaleFor = 24 * time.Hour

	// EnrichmentSourceKey and EnrichmentStaleKey are reserved keys the
	// Enricher sets itself after parsing, overwriting whatever the source
	// sent, so a source cannot claim to be something it is not.
	EnrichmentSourceKey = "_source"
	EnrichmentStaleKey  = "_stale"
	// EnrichmentSourceOperator names the only source this enricher has: the
	// operator-configured ALERT_ENRICHMENT_URL endpoint.
	EnrichmentSourceOperator = "operator"
)

// Enricher fetches operator-supplied alert context. The response is kept as a
// separate JSON object so untrusted fields cannot become notification template
// text or override the alert's own fields.
//
// Latency budget: the lookup runs inside alert creation, so Fetch is bounded
// by the timeout given to NewEnricher (default 2s) as a hard deadline on the
// whole exchange, connection and body read included. A slow or failing source
// costs at most that budget and the alert is created unenriched.
//
// Cache: process-local, bounded to maxEntries, TTL-expired, and invalidated
// per contract with Invalidate. There is no cross-replica invalidation
// channel in this codebase to reuse, so a replica picks up a changed label
// when its own entry expires; the TTL is the stated staleness bound.
type Enricher struct {
	endpoint   string
	client     *http.Client
	timeout    time.Duration
	ttl        time.Duration
	staleFor   time.Duration
	maxEntries int
	now        func() time.Time
	mu         sync.Mutex
	cache      map[string]cacheEntry
}

type cacheEntry struct {
	value   json.RawMessage
	expires time.Time
}

// NewEnricher creates an enricher. An empty endpoint disables enrichment.
func NewEnricher(endpoint string, timeout, ttl time.Duration) (*Enricher, error) {
	if endpoint == "" {
		return nil, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid ALERT_ENRICHMENT_URL: want an absolute http or https URL")
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Enricher{
		endpoint: endpoint, client: &http.Client{Timeout: timeout}, timeout: timeout, ttl: ttl,
		staleFor: DefaultEnrichmentStaleFor, maxEntries: DefaultEnrichmentCacheEntries,
		now: time.Now, cache: make(map[string]cacheEntry),
	}, nil
}

// Invalidate drops one contract's cached entry.
func (e *Enricher) Invalidate(contractID string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	delete(e.cache, contractID)
	e.mu.Unlock()
}

// Len reports the number of cached entries.
func (e *Enricher) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.cache)
}

func withFlags(value json.RawMessage, stale bool) json.RawMessage {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return value
	}
	object[EnrichmentSourceKey], _ = json.Marshal(EnrichmentSourceOperator)
	if stale {
		object[EnrichmentStaleKey] = json.RawMessage("true")
	} else {
		delete(object, EnrichmentStaleKey)
	}
	out, _ := json.Marshal(object)
	return out
}

// Fetch returns a bounded JSON object for the contract, or an error. Callers
// should treat errors as a soft failure and deliver the original alert. When
// the source fails but a previous answer is still within the stale window,
// that answer is returned marked "_stale": true instead of an error.
func (e *Enricher) Fetch(ctx context.Context, contractID, eventName string) (json.RawMessage, error) {
	if e == nil || e.endpoint == "" {
		return nil, nil
	}
	key := contractID
	e.mu.Lock()
	cached, have := e.cache[key]
	if have && e.now().Before(cached.expires) {
		value := withFlags(cached.value, false)
		e.mu.Unlock()
		return value, nil
	}
	e.mu.Unlock()

	value, err := e.fetchRemote(ctx, contractID, eventName)
	if err != nil {
		if have && e.now().Before(cached.expires.Add(e.staleFor)) {
			return withFlags(cached.value, true), nil
		}
		return nil, err
	}
	e.mu.Lock()
	e.store(key, cacheEntry{value: value, expires: e.now().Add(e.ttl)})
	e.mu.Unlock()
	return withFlags(value, false), nil
}

// store inserts an entry, evicting to stay within maxEntries: entries past
// their stale window first, then the one closest to expiry. The caller holds
// e.mu.
func (e *Enricher) store(key string, entry cacheEntry) {
	if _, exists := e.cache[key]; !exists && len(e.cache) >= e.maxEntries {
		now := e.now()
		var oldestKey string
		var oldest time.Time
		for k, v := range e.cache {
			if now.After(v.expires.Add(e.staleFor)) {
				delete(e.cache, k)
				continue
			}
			if oldestKey == "" || v.expires.Before(oldest) {
				oldestKey, oldest = k, v.expires
			}
		}
		if len(e.cache) >= e.maxEntries && oldestKey != "" {
			delete(e.cache, oldestKey)
		}
	}
	e.cache[key] = entry
}

func (e *Enricher) fetchRemote(ctx context.Context, contractID, eventName string) (json.RawMessage, error) {
	// A hard deadline on the whole exchange, independent of the caller's
	// context: the alert path's latency is what this protects.
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	u, err := url.Parse(e.endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("contract_id", contractID)
	q.Set("event_name", eventName)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("enrichment source returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEnrichmentBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxEnrichmentBytes {
		return nil, fmt.Errorf("enrichment response exceeds %d bytes", maxEnrichmentBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, fmt.Errorf("enrichment response must be a JSON object")
	}
	value, _ := json.Marshal(object)
	return value, nil
}
