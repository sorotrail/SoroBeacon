package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// MaxTopicRegexPatternLength bounds the pattern a topic_regex rule may
// carry. Validate rejects anything longer, so a pathological pattern (or a
// rule built from untrusted paste) is rejected at create time by the API
// instead of stalling the poller's per-event evaluation later. The limit
// sits far above any sane event-family pattern while keeping a worst-case
// regexp compile and match cheap enough to run inside the ingest loop.
const MaxTopicRegexPatternLength = 512

// topicRegexParams configure the topic_regex rule.
//
//	{
//	  "pattern": "^swap_",   // required: RE2 regular expression
//	  "position": 0          // optional: match only this topic; omitted = any topic
//	}
//
// Real contracts emit families of events (swap_exact_in, swap_exact_out,
// pool_deposit, ...) and a regex rule covers the family — and the long tail
// of custom contracts whose topic conventions SoroBeacon cannot know in
// advance — with one rule instead of one event_emitted rule per name.
type topicRegexParams struct {
	Pattern  string `json:"pattern"`
	Position *int   `json:"position"`
}

// TopicRegex matches when the configured regular expression matches a
// decoded topic — the topic at the given position, or any topic when
// position is omitted.
//
// Evaluation compiles the pattern on every call, so the compiled
// *regexp.Regexp is memoised per params blob in a bounded cache: the poller
// evaluates the same rule params once per event, and recompiling (or
// re-parsing the JSON) per event would dominate the rule's cost. The cache
// is keyed by the raw params, capped, and safe for concurrent use — the
// poller evaluates rules concurrently across monitors.
type TopicRegex struct {
	cache regexCache
}

// regexCache memoises compiled patterns per params blob. Entries are
// immutable (*regexp.Regexp is safe for concurrent use), so a hit needs no
// validation beyond the key match.
type regexCache struct {
	mu sync.RWMutex
	m  map[string]regexCacheEntry
}

// regexCacheMax bounds the cache: one entry per distinct rule params, and
// rules are deleted along with their monitors, so a small cap is plenty. An
// eviction is merely a lost optimisation — the entry is recompiled on the
// next evaluation.
const regexCacheMax = 256

type regexCacheEntry struct {
	re       *regexp.Regexp
	position *int
}

func (c *regexCache) get(params string) (regexCacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[params]
	return e, ok
}

func (c *regexCache) put(params string, e regexCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]regexCacheEntry, 8)
	}
	if len(c.m) >= regexCacheMax {
		// Drop one arbitrary entry; correctness does not depend on which.
		for k := range c.m {
			delete(c.m, k)
			break
		}
	}
	c.m[params] = e
}

// compilePattern parses the params and compiles the pattern. It is the
// shared path for Validate and Evaluate: the API compiles at create time to
// reject bad rules early, and evaluation compiles against the same checks,
// so a pattern that passed Validate behaves identically in the poller.
// Position is only range-checked in Validate: at evaluation time a position
// outside the event's topic list simply matches nothing.
func compilePattern(params json.RawMessage) (*regexp.Regexp, *int, error) {
	p, err := parseTopicRegex(params)
	if err != nil {
		return nil, nil, err
	}
	if p.Pattern == "" {
		return nil, nil, FieldError{Field: "pattern", Reason: "topic_regex: pattern is required"}
	}
	if len(p.Pattern) > MaxTopicRegexPatternLength {
		return nil, nil, FieldError{
			Field:  "pattern",
			Reason: fmt.Sprintf("topic_regex: pattern is %d bytes; the maximum is %d", len(p.Pattern), MaxTopicRegexPatternLength),
		}
	}
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("topic_regex: invalid pattern: %w", err)
	}
	return re, p.Position, nil
}

func (*TopicRegex) Validate(params json.RawMessage) error {
	// Compile here so an invalid pattern is a create-time rejection by the
	// API rather than a per-event failure in the poller.
	if _, _, err := compilePattern(params); err != nil {
		return err
	}
	p, err := parseTopicRegex(params)
	if err != nil {
		return err
	}
	if p.Position != nil && *p.Position < 0 {
		return FieldError{
			Field:  "position",
			Reason: fmt.Sprintf("topic_regex: position %d is negative", *p.Position),
		}
	}
	return nil
}

func (t *TopicRegex) Evaluate(_ context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	// The same params blob is evaluated once per event for the life of the
	// rule, so a cache hit skips both the JSON parse and the compile.
	if e, ok := t.cache.get(string(params)); ok {
		return t.match(e.re, e.position, ev)
	}
	re, position, err := compilePattern(params)
	if err != nil {
		return false, err
	}
	t.cache.put(string(params), regexCacheEntry{re: re, position: position})
	return t.match(re, position, ev)
}

// match runs the compiled pattern against the event's topics. A position
// beyond either end of the topic list means no match, not an error: the
// event simply does not carry that topic.
func (*TopicRegex) match(re *regexp.Regexp, position *int, ev *stellar.DecodedEvent) (bool, error) {
	if position != nil && (*position < 0 || *position >= len(ev.Topics)) {
		return false, nil
	}

	// A position beyond either end of the topic list means no match, not an
	// error: the event simply does not carry that topic.
	if position != nil && (*position < 0 || *position >= len(ev.Topics)) {
		return false, nil
	}

	for i, topic := range ev.Topics {
		if position != nil && i != *position {
			continue
		}
		if s, ok := topicString(topic); ok && re.MatchString(s) {
			return true, nil
		}
	}
	return false, nil
}

// topicString renders a decoded topic as the string the pattern matches
// against. Topics arrive in two shapes (both handled by the same logic as
// token_event's addr helper): the XDR decode path emits bare strings, while
// the RPC's xdrFormat:"json" path emits single-key wrappers like
// {"symbol":"transfer"} or {"address":"G..."}. Anything else — integers,
// bytes, vectors — is not a string topic and simply does not match rather
// than erroring.
func topicString(topic any) (string, bool) {
	switch v := topic.(type) {
	case string:
		return v, true
	case map[string]any:
		for _, key := range []string{"symbol", "string", "address"} {
			if s, ok := v[key].(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

func parseTopicRegex(params json.RawMessage) (topicRegexParams, error) {
	var p topicRegexParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("topic_regex: invalid params: %w", err)
	}
	return p, nil
}
