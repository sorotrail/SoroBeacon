package graphql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

func parseComplexityDocument(t *testing.T, doc string) *ast.QueryDocument {
	t.Helper()
	parsed, err := parser.ParseQuery(&ast.Source{Input: doc})
	require.NoError(t, err)
	return parsed
}

func TestCalculateQueryComplexity(t *testing.T) {
	limitedDoc := `query List($limit: Int) { monitors(limit: $limit) { edges { node { id } } } }`
	tests := []struct {
		name string
		doc  *ast.QueryDocument
		vars map[string]any
		want int
	}{
		{
			name: "nil document scores zero",
			doc:  nil,
			want: 0,
		},
		{
			name: "flat selections add one per field",
			doc:  parseComplexityDocument(t, `query Stats { stats { monitors rules } }`),
			want: 3,
		},
		{
			name: "limit variable multiplies nested fields",
			doc:  parseComplexityDocument(t, limitedDoc),
			vars: map[string]any{"limit": 50},
			want: 151,
		},
		{
			name: "invalid limit falls back to one",
			doc:  parseComplexityDocument(t, limitedDoc),
			vars: map[string]any{"limit": "not-a-number"},
			want: 4,
		},
		{
			name: "non-positive limit falls back to one",
			doc:  parseComplexityDocument(t, limitedDoc),
			vars: map[string]any{"limit": 0},
			want: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, CalculateQueryComplexity(tt.doc, tt.vars))
		})
	}
}
