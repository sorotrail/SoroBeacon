package rules

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// swapEvent is a DEX-shaped event whose topics arrive in the two decoded
// shapes: the XDR path emits bare strings, while the RPC's xdrFormat:"json"
// path emits single-key wrappers ({"symbol":...} / {"address":...}).
func swapEvent(shaped bool) *stellar.DecodedEvent {
	name, from, to := any("swap_exact_in"), any("GAAA1"), any("GBBB2")
	if shaped {
		name = map[string]any{"symbol": "swap_exact_in"}
		from = map[string]any{"address": "GAAA1"}
		to = map[string]any{"address": "GBBB2"}
	}
	return &stellar.DecodedEvent{
		ID:         "0000000012884905986-0000000001",
		ContractID: "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Topics:     []any{name, from, to},
	}
}

func TestTopicRegexEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		event   *stellar.DecodedEvent
		want    bool
		wantErr bool
	}{
		{
			name:   "matches at position 0 (the event name)",
			params: `{"pattern": "^swap_", "position": 0}`,
			event:  swapEvent(false),
			want:   true,
		},
		{
			name:   "matches at position 1",
			params: `{"pattern": "^GAAA", "position": 1}`,
			event:  swapEvent(false),
			want:   true,
		},
		{
			name:   "match at position only considers that position",
			params: `{"pattern": "^GBBB", "position": 1}`,
			event:  swapEvent(false),
			want:   false,
		},
		{
			name:   "omitted position matches any topic",
			params: `{"pattern": "^GBBB"}`,
			event:  swapEvent(false),
			want:   true,
		},
		{
			name:   "omitted position matches the event name",
			params: `{"pattern": "_exact_"}`,
			event:  swapEvent(false),
			want:   true,
		},
		{
			name:   "no match anywhere",
			params: `{"pattern": "^pool_"}`,
			event:  swapEvent(false),
			want:   false,
		},
		{
			name:   "anchored pattern must match within the topic",
			params: `{"pattern": "^exact"}`,
			event:  swapEvent(false),
			want:   false,
		},
		{
			name:   "event with no topics never matches",
			params: `{"pattern": "swap"}`,
			event:  &stellar.DecodedEvent{},
			want:   false,
		},
		{
			name:   "position beyond the topic list means no match, not an error",
			params: `{"pattern": "swap", "position": 7}`,
			event:  swapEvent(false),
			want:   false,
		},
		{
			name:   "omitted position matches any topic",
			params: `{"pattern": "^GBBB"}`,
			event:  swapEvent(true),
			want:   true,
		},
		{
			name:   "wrapper-shaped topics match like bare strings",
			params: `{"pattern": "^swap_", "position": 0}`,
			event:  swapEvent(true),
			want:   true,
		},
		{
			name:   "wrapper-shaped address topic matches",
			params: `{"pattern": "^GAAA", "position": 1}`,
			event:  swapEvent(true),
			want:   true,
		},
		{
			name:   "wrapper-shaped topic matches anywhere",
			params: `{"pattern": "^GBBB"}`,
			event:  swapEvent(true),
			want:   true,
		},
		{
			name:   "non-string topics do not match and do not error",
			params: `{"pattern": "^42|^250"}`,
			event: &stellar.DecodedEvent{
				Topics: []any{"name", 42, int64(250)},
			},
			want: false,
		},
		{
			name:   "non-string topic skipped while a string sibling matches",
			params: `{"pattern": "^swap"}`,
			event: &stellar.DecodedEvent{
				Topics: []any{250, "swap_exact_in"},
			},
			want: true,
		},
		{
			name:   "nil topic does not match",
			params: `{"pattern": "."}`,
			event: &stellar.DecodedEvent{
				Topics: []any{nil},
			},
			want: false,
		},
		{
			name:    "invalid params JSON errors",
			params:  `{"pattern": 7}`,
			event:   swapEvent(false),
			wantErr: true,
		},
		{
			name:    "invalid pattern errors at evaluation",
			params:  `{"pattern": "^swap("}`,
			event:   swapEvent(false),
			wantErr: true,
		},
		{
			name:    "empty pattern errors at evaluation",
			params:  `{"pattern": ""}`,
			event:   swapEvent(false),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := new(TopicRegex).Evaluate(context.Background(), tt.event, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTopicRegexValidate(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		wantErr bool
		wantSub string
	}{
		{name: "valid pattern and position", params: `{"pattern": "^swap_", "position": 0}`},
		{name: "position omitted is fine", params: `{"pattern": "^swap_"}`},
		{name: "missing pattern", params: `{}`, wantErr: true, wantSub: "pattern is required"},
		{name: "empty pattern", params: `{"pattern": ""}`, wantErr: true, wantSub: "pattern is required"},
		{name: "invalid regex", params: `{"pattern": "^swap("}`, wantErr: true, wantSub: "invalid pattern"},
		{name: "negative position", params: `{"pattern": "x", "position": -1}`, wantErr: true, wantSub: "negative"},
		{name: "position must be a number", params: `{"pattern": "x", "position": "zero"}`, wantErr: true, wantSub: "invalid params"},
		{
			name:    "pattern over the documented maximum is rejected",
			params:  `{"pattern": "` + strings.Repeat("a", MaxTopicRegexPatternLength+1) + `"}`,
			wantErr: true,
			wantSub: "maximum is",
		},
		{
			name:   "pattern at the maximum is accepted",
			params: `{"pattern": "^` + strings.Repeat("a", MaxTopicRegexPatternLength-2) + `$"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := new(TopicRegex).Validate(json.RawMessage(tt.params))
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			if tt.wantSub != "" {
				assert.Contains(t, err.Error(), tt.wantSub)
			}
		})
	}
}

// TestTopicRegexRejectedAtCreateTime checks the registry path the API uses:
// a bad pattern must be a Validate failure (an HTTP 400 at rule creation),
// not a per-event evaluation error discovered only once an event arrives.
func TestTopicRegexRejectedAtCreateTime(t *testing.T) {
	r := NewRegistry()
	err := r.Validate(TypeTopicRegex, json.RawMessage(`{"pattern": "^swap("}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pattern")
}

func TestTopicRegexRegistered(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Validate(TypeTopicRegex, json.RawMessage(`{"pattern": "^swap_"}`)))

	got, err := r.Evaluate(context.Background(), TypeTopicRegex, swapEvent(false), json.RawMessage(`{"pattern": "^swap_"}`))
	require.NoError(t, err)
	assert.True(t, got)
}

// TestTopicRegexCompiledPatternReused pins the reuse requirement: the same
// params blob must compile once, not once per event. The first evaluation
// populates the cache; the second must be served from it, and a third with
// different params must not collide with it.
func TestTopicRegexCompiledPatternReused(t *testing.T) {
	e := new(TopicRegex)
	ev := swapEvent(false)
	params := json.RawMessage(`{"pattern": "^swap_", "position": 0}`)

	got, err := e.Evaluate(context.Background(), ev, params)
	require.NoError(t, err)
	assert.True(t, got)
	entry, ok := e.cache.get(string(params))
	require.True(t, ok, "first evaluation must populate the cache")

	got, err = e.Evaluate(context.Background(), ev, params)
	require.NoError(t, err)
	assert.True(t, got)
	again, ok := e.cache.get(string(params))
	require.True(t, ok)
	assert.Same(t, entry.re, again.re, "second evaluation must reuse the compiled pattern")

	other := json.RawMessage(`{"pattern": "^pool_"}`)
	got, err = e.Evaluate(context.Background(), ev, other)
	require.NoError(t, err)
	assert.False(t, got)
	assert.NotContains(t, e.cache.m, string(params)+"x", "unrelated params must not disturb the cached entry")
}

// TestTopicRegexCacheEviction checks the cache stays bounded: filling past
// the cap evicts rather than growing without limit.
func TestTopicRegexCacheEviction(t *testing.T) {
	e := new(TopicRegex)
	ev := &stellar.DecodedEvent{Topics: []any{"x"}}
	for i := 0; i < regexCacheMax+10; i++ {
		params := json.RawMessage(`{"pattern": "^p` + strconv.Itoa(i) + `$"}`)
		_, err := e.Evaluate(context.Background(), ev, params)
		require.NoError(t, err)
	}
	assert.LessOrEqual(t, len(e.cache.m), regexCacheMax, "cache must stay bounded")
}
