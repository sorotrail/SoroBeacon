// Package secrets resolves external secret references embedded in channel
// configuration.
//
// A channel config value may be written as a reference:
//
//	${secret:<provider>:<path>[#<key>]}
//
// Resolution happens when a notifier is constructed, so every channel type
// benefits without individual changes. A string that is not exactly a
// reference is treated as a literal, so existing channels keep working
// untouched.
//
// Resolved values stay in memory and are never written back to the database:
// the stored config keeps the reference. Errors name the reference but never
// the resolved value, and a resolution failure fails the send rather than
// delivering an unresolved placeholder.
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	refPrefix = "${secret:"
	refSuffix = "}"
)

// DefaultTTL bounds how long a resolved value is reused. It is deliberately
// short: long enough that a burst of alerts does not hit the provider once
// per alert, short enough that a rotated secret is picked up without a
// restart. Documented in docs/channels/secrets.md.
const DefaultTTL = 5 * time.Minute

// Reference is a parsed ${secret:provider:path#key} value. Key is optional.
type Reference struct {
	Provider string
	Path     string
	Key      string
}

// String renders the reference in its canonical form. It is used for error
// messages and never contains a resolved value.
func (r Reference) String() string {
	s := refPrefix + r.Provider + ":" + r.Path
	if r.Key != "" {
		s += "#" + r.Key
	}
	return s + refSuffix
}

// ParseReference parses s as a single reference. It reports ok=false when s
// is not exactly one well-formed reference — the caller then treats s as a
// literal. Malformed references (an empty provider or path, an unknown
// character) are literals too, so a typo cannot silently resolve to the
// wrong secret.
func ParseReference(s string) (Reference, bool) {
	if !strings.HasPrefix(s, refPrefix) || !strings.HasSuffix(s, refSuffix) {
		return Reference{}, false
	}
	inner := s[len(refPrefix) : len(s)-len(refSuffix)]
	if inner == "" || strings.Contains(inner, refPrefix) {
		return Reference{}, false
	}
	provider, rest, ok := strings.Cut(inner, ":")
	if !ok || !validName(provider) {
		return Reference{}, false
	}
	path, key, _ := strings.Cut(rest, "#")
	if path == "" || strings.ContainsAny(path, " \t\n") {
		return Reference{}, false
	}
	if key != "" && strings.ContainsAny(key, " \t\n#") {
		return Reference{}, false
	}
	return Reference{Provider: provider, Path: path, Key: key}, true
}

// IsReference reports whether s is a well-formed reference.
func IsReference(s string) bool {
	_, ok := ParseReference(s)
	return ok
}

// validName constrains a provider scheme to the characters a reference can
// carry: lowercase letters, digits, underscore and hyphen. Restricting it
// keeps the syntax unambiguous and the error messages readable.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Provider fetches one secret from an external secret store. The interface
// is deliberately tiny so a third provider is a contained change: implement
// Scheme and Fetch and register it on a Resolver.
type Provider interface {
	// Scheme is the provider name used in references (the part after
	// "secret:"), e.g. "env" or "vault".
	Scheme() string
	// Fetch returns the secret at path. key selects one field of a
	// structured secret (a Vault KV entry, for example) and may be empty
	// when the secret is a single value. Implementations must return an
	// error that names the path but never the resolved value or the raw
	// response body.
	Fetch(ctx context.Context, path, key string) (string, error)
}

// Resolver resolves references through registered providers and caches the
// results for a TTL.
type Resolver struct {
	mu        sync.Mutex
	providers map[string]Provider
	cache     map[string]cacheEntry
	ttl       time.Duration
	now       func() time.Time
}

type cacheEntry struct {
	value   string
	expires time.Time
}

// NewResolver builds a Resolver from providers. The zero-provider resolver
// still treats references as literals, so it is inert until a provider is
// registered.
func NewResolver(providers ...Provider) *Resolver {
	r := &Resolver{
		providers: map[string]Provider{},
		cache:     map[string]cacheEntry{},
		ttl:       DefaultTTL,
		now:       time.Now,
	}
	for _, p := range providers {
		r.Register(p)
	}
	return r
}

// Register adds (or replaces) a provider.
func (r *Resolver) Register(p Provider) {
	r.providers[p.Scheme()] = p
}

// WithTTL sets the cache lifetime. A non-positive duration disables caching.
func (r *Resolver) WithTTL(d time.Duration) *Resolver {
	r.ttl = d
	return r
}

// Schemes returns the registered provider names.
func (r *Resolver) Schemes() []string {
	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}
	return out
}

// Invalidate drops every cached value. The API calls it when a channel is
// created, updated or deleted so a corrected secret is picked up
// immediately rather than after the TTL.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = map[string]cacheEntry{}
}

// ResolveString returns the literal s unless it is a reference, in which
// case the referenced secret is resolved and returned.
func (r *Resolver) ResolveString(ctx context.Context, s string) (string, error) {
	ref, ok := ParseReference(s)
	if !ok {
		return s, nil
	}
	return r.Resolve(ctx, ref)
}

// Resolve fetches the secret a reference points at, using the cache when a
// fresh entry exists. Errors name the reference and wrap the provider's
// error; the resolved value is never part of an error.
func (r *Resolver) Resolve(ctx context.Context, ref Reference) (string, error) {
	p, ok := r.providers[ref.Provider]
	if !ok {
		return "", fmt.Errorf("secret reference %s: unknown provider %q", ref, ref.Provider)
	}
	if v, ok := r.lookup(ref); ok {
		return v, nil
	}
	v, err := p.Fetch(ctx, ref.Path, ref.Key)
	if err != nil {
		return "", fmt.Errorf("secret reference %s: %w", ref, err)
	}
	r.store(ref, v)
	return v, nil
}

func (r *Resolver) cacheKey(ref Reference) string {
	return ref.Provider + "\x00" + ref.Path + "\x00" + ref.Key
}

func (r *Resolver) lookup(ref Reference) (string, bool) {
	if r.ttl <= 0 {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[r.cacheKey(ref)]
	if !ok || !r.now().Before(e.expires) {
		return "", false
	}
	return e.value, true
}

func (r *Resolver) store(ref Reference, value string) {
	if r.ttl <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[r.cacheKey(ref)] = cacheEntry{value: value, expires: r.now().Add(r.ttl)}
}

// ResolveConfig walks a channel's JSON config and resolves every string leaf
// that is a reference, returning a new document. The input is never
// mutated, so the caller's stored config (which keeps the references) is
// untouched. A config that is not valid JSON is returned unchanged: the
// notifier constructor reports the syntax error with its usual message.
//
// Numbers are decoded with json.Number so a large integer (an i128 amount in
// a template parameter, say) survives the round trip without becoming a
// float.
func (r *Resolver) ResolveConfig(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw, nil
	}
	out, err := r.resolveValue(ctx, v)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw, nil
	}
	return b, nil
}

func (r *Resolver) resolveValue(ctx context.Context, v any) (any, error) {
	switch t := v.(type) {
	case string:
		return r.ResolveString(ctx, t)
	case map[string]any:
		for k, val := range t {
			nv, err := r.resolveValue(ctx, val)
			if err != nil {
				return nil, err
			}
			t[k] = nv
		}
		return t, nil
	case []any:
		for i, val := range t {
			nv, err := r.resolveValue(ctx, val)
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	default:
		return v, nil
	}
}
