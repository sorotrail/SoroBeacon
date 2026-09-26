package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// recordingEvaluator is a leaf used by the short-circuit tests: it records how
// many times it was evaluated and returns a fixed result or error, so a
// composite that short-circuits leaves calls at zero.
type recordingEvaluator struct {
	value bool
	err   error
	calls int
}

func (r *recordingEvaluator) Evaluate(context.Context, *stellar.DecodedEvent, json.RawMessage) (bool, error) {
	r.calls++
	return r.value, r.err
}

func (r *recordingEvaluator) Validate(json.RawMessage) error { return nil }

func TestCompositeEvaluate(t *testing.T) {
	ev := transferEvent(50)
	tests := []struct {
		name    string
		params  string
		want    bool
		wantErr bool
	}{
		{
			name: "and both true",
			params: `{"op":"and","rules":[
				{"type":"event_emitted","params":{"event_name":"transfer"}},
				{"type":"value_threshold","params":{"comparison":"gt","threshold":1,"value_path":"amount"}}
			]}`,
			want: true,
		},
		{
			name: "and one false",
			params: `{"op":"and","rules":[
				{"type":"event_emitted","params":{"event_name":"transfer"}},
				{"type":"value_threshold","params":{"comparison":"gt","threshold":1000,"value_path":"amount"}}
			]}`,
			want: false,
		},
		{
			name: "or one true",
			params: `{"op":"or","rules":[
				{"type":"event_emitted","params":{"event_name":"mint"}},
				{"type":"value_threshold","params":{"comparison":"gt","threshold":1,"value_path":"amount"}}
			]}`,
			want: true,
		},
		{
			name: "or both false",
			params: `{"op":"or","rules":[
				{"type":"event_emitted","params":{"event_name":"mint"}},
				{"type":"value_threshold","params":{"comparison":"gt","threshold":1000,"value_path":"amount"}}
			]}`,
			want: false,
		},
		{
			name:   "not inverts a false child",
			params: `{"op":"not","rules":[{"type":"event_emitted","params":{"event_name":"mint"}}]}`,
			want:   true,
		},
		{
			name:   "not inverts a true child",
			params: `{"op":"not","rules":[{"type":"event_emitted","params":{"event_name":"transfer"}}]}`,
			want:   false,
		},
		{
			name: "nested composites combine",
			params: `{"op":"and","rules":[
				{"type":"composite","params":{"op":"or","rules":[
					{"type":"event_emitted","params":{"event_name":"mint"}},
					{"type":"event_emitted","params":{"event_name":"transfer"}}
				]}},
				{"type":"value_threshold","params":{"comparison":"gt","threshold":1,"value_path":"amount"}}
			]}`,
			want: true,
		},
		{
			name:    "malformed params error",
			params:  `{"op":`,
			wantErr: true,
		},
		{
			name:    "unknown op error",
			params:  `{"op":"xor","rules":[{"type":"event_emitted","params":{"event_name":"transfer"}}]}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry()
			got, err := reg.Evaluate(context.Background(), TypeComposite, ev, json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCompositeValidate(t *testing.T) {
	tests := []struct {
		name       string
		params     string
		wantErr    bool
		wantSubstr string // a substring the error message must contain
	}{
		{
			name: "valid and",
			params: `{"op":"and","rules":[
				{"type":"event_emitted","params":{"event_name":"transfer"}},
				{"type":"token_event","params":{"event":"transfer","min_amount":"1000000"}}
			]}`,
		},
		{
			name:       "op required",
			params:     `{"rules":[{"type":"event_emitted","params":{"event_name":"transfer"}}]}`,
			wantErr:    true,
			wantSubstr: "composite: op is required",
		},
		{
			name:       "unknown op",
			params:     `{"op":"xor","rules":[{"type":"event_emitted","params":{"event_name":"transfer"}}]}`,
			wantErr:    true,
			wantSubstr: `unknown op "xor"`,
		},
		{
			name:       "and needs a child",
			params:     `{"op":"and","rules":[]}`,
			wantErr:    true,
			wantSubstr: `op "and" requires at least one`,
		},
		{
			name:       "not takes exactly one",
			params:     `{"op":"not","rules":[{"type":"event_emitted","params":{"event_name":"transfer"}},{"type":"event_emitted","params":{"event_name":"mint"}}]}`,
			wantErr:    true,
			wantSubstr: `op "not" requires exactly one`,
		},
		{
			name:       "child type required",
			params:     `{"op":"and","rules":[{"params":{}}]}`,
			wantErr:    true,
			wantSubstr: "rules[0].type",
		},
		{
			name:       "unknown child type",
			params:     `{"op":"and","rules":[{"type":"nope","params":{}}]}`,
			wantErr:    true,
			wantSubstr: `unknown rule type "nope"`,
		},
		{
			name:       "child validation failure names its path",
			params:     `{"op":"and","rules":[{"type":"event_emitted","params":{"event_name":"transfer"}},{"type":"value_threshold","params":{"comparison":"gt"}}]}`,
			wantErr:    true,
			wantSubstr: "rules[1].params",
		},
		{
			name:       "grandchild failure names the full path",
			params:     `{"op":"and","rules":[{"type":"composite","params":{"op":"or","rules":[{"type":"value_threshold","params":{"comparison":"gt"}}]}}]}`,
			wantErr:    true,
			wantSubstr: "rules[0].params.rules[0].params",
		},
		{
			name:       "malformed params",
			params:     `{`,
			wantErr:    true,
			wantSubstr: "invalid params",
		},
		{
			name:       "cooldown still validated through the registry",
			params:     `{"op":"not","rules":[{"type":"event_emitted","params":{"event_name":"mint"}}],"cooldown":"nope"}`,
			wantErr:    true,
			wantSubstr: "cooldown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry()
			err := reg.Validate(TypeComposite, json.RawMessage(tt.params))
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantSubstr)
		})
	}
}

func TestCompositeShortCircuit(t *testing.T) {
	newRegistry := func() (*Registry, *recordingEvaluator, *recordingEvaluator, *recordingEvaluator) {
		reg := NewRegistry()
		yes := &recordingEvaluator{value: true}
		no := &recordingEvaluator{value: false}
		boom := &recordingEvaluator{err: errors.New("boom")}
		reg.Register("test_true", yes)
		reg.Register("test_false", no)
		reg.Register("test_error", boom)
		return reg, yes, no, boom
	}

	tests := []struct {
		name      string
		params    string
		want      bool
		wantErr   bool
		wantCalls int // calls to the trailing test_error child
	}{
		{
			name:      "and stops at the first false",
			params:    `{"op":"and","rules":[{"type":"test_false","params":{}},{"type":"test_error","params":{}}]}`,
			want:      false,
			wantCalls: 0,
		},
		{
			name:      "and evaluates the error child once the guard passes",
			params:    `{"op":"and","rules":[{"type":"test_true","params":{}},{"type":"test_error","params":{}}]}`,
			wantErr:   true,
			wantCalls: 1,
		},
		{
			name:      "or stops at the first true",
			params:    `{"op":"or","rules":[{"type":"test_true","params":{}},{"type":"test_error","params":{}}]}`,
			want:      true,
			wantCalls: 0,
		},
		{
			name:      "or evaluates the error child only after a false",
			params:    `{"op":"or","rules":[{"type":"test_false","params":{}},{"type":"test_error","params":{}}]}`,
			wantErr:   true,
			wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, _, _, boom := newRegistry()
			got, err := reg.Evaluate(context.Background(), TypeComposite, transferEvent(50), json.RawMessage(tt.params))
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
			assert.Equal(t, tt.wantCalls, boom.calls, "trailing child call count")
		})
	}
}

// nestedComposite builds levels composites wrapped around a matching leaf, so
// a depth-limit test does not have to hand-write the nesting.
func nestedComposite(levels int) json.RawMessage {
	// The innermost composite wraps a plain leaf; each further level wraps the
	// level below as a composite child, so levels counts composite levels.
	params := `{"op":"and","rules":[{"type":"event_emitted","params":{"event_name":"transfer"}}]}`
	for i := 1; i < levels; i++ {
		params = `{"op":"and","rules":[{"type":"composite","params":` + params + `}]}`
	}
	return json.RawMessage(params)
}

func TestCompositeNestingDepth(t *testing.T) {
	reg := NewRegistry()

	// The documented limit is five levels; the test pins the literal rather
	// than the constant so changing the limit without updating the docs fails.
	const documentedLimit = 5
	require.Equal(t, documentedLimit, maxCompositeDepth, "documented nesting limit")

	// Five composite levels (the documented maximum) are accepted.
	require.NoError(t, reg.Validate(TypeComposite, nestedComposite(documentedLimit)))

	// A sixth composite level is rejected, and the error names the offending
	// child's params path so the too-deep level is unambiguous.
	err := reg.Validate(TypeComposite, nestedComposite(documentedLimit+1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("more than %d levels", documentedLimit))
	assert.Contains(t, err.Error(), ".params")
}

func TestCompositeUnknownChildType(t *testing.T) {
	reg := NewRegistry()
	params := json.RawMessage(`{"op":"or","rules":[{"type":"does_not_exist","params":{}}]}`)

	// Validation rejects the unknown type rather than letting it reach
	// evaluation, where it would otherwise be a panic.
	err := reg.Validate(TypeComposite, params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown rule type "does_not_exist"`)
	assert.Contains(t, err.Error(), "rules[0].params")

	// And if it did somehow reach evaluation, it is an error, not a panic.
	require.NotPanics(t, func() {
		_, err := reg.Evaluate(context.Background(), TypeComposite, transferEvent(50), params)
		require.Error(t, err)
	})
}

// TestCompositeRegisteredViaNewRegistry confirms the composite is reachable
// through the default registry with its child types resolved.
func TestCompositeRegisteredViaNewRegistry(t *testing.T) {
	reg := NewRegistry()
	assert.Contains(t, reg.Types(), TypeComposite)

	params := json.RawMessage(`{"op":"and","rules":[
		{"type":"event_emitted","params":{"event_name":"transfer"}},
		{"type":"value_threshold","params":{"comparison":"gte","threshold":50,"value_path":"amount"}}
	]}`)
	require.NoError(t, reg.Validate(TypeComposite, params))
	got, err := reg.Evaluate(context.Background(), TypeComposite, transferEvent(50), params)
	require.NoError(t, err)
	assert.True(t, got)
}

// TestCompositeErrorPathsAreRootedAndSelfContained guards the shape of the
// detail fields the API surfaces: every child error is rooted at the parent
// params path, never left relative to the child.
func TestCompositeErrorPathsAreRootedAndSelfContained(t *testing.T) {
	reg := NewRegistry()
	params := json.RawMessage(`{"op":"and","rules":[
		{"type":"event_emitted","params":{"event_name":"transfer"}},
		{"type":"value_threshold","params":{"comparison":"gt"}},
		{"type":"composite","params":{"op":"not","rules":[
			{"type":"token_event","params":{}}
		]}}
	]}`)
	err := reg.Validate(TypeComposite, params)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "rules[1].params")
	assert.Contains(t, msg, "rules[2].params.rules[0].params")
}
