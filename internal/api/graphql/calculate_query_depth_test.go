package graphql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

func parseDepthDocument(t *testing.T, doc string) *ast.QueryDocument {
	t.Helper()
	parsed, err := parser.ParseQuery(&ast.Source{Input: doc})
	if err != nil {
		t.Fatalf("parse depth fixture: %v", err)
	}
	return parsed
}

func TestCalculateQueryDepth(t *testing.T) {
	tests := []struct {
		name string
		doc  *ast.QueryDocument
		want int
	}{
		{
			name: "nil document",
			doc:  nil,
			want: 0,
		},
		{
			name: "document without operations",
			doc:  parseDepthDocument(t, `fragment StatsFields on Stats { monitors }`),
			want: 0,
		},
		{
			name: "a single field",
			doc:  parseDepthDocument(t, `{ stats }`),
			want: 1,
		},
		{
			name: "nested fields",
			doc:  parseDepthDocument(t, `{ monitors { edges { node { id } } } }`),
			want: 4,
		},
		{
			name: "the deepest branch wins",
			doc:  parseDepthDocument(t, `{ stats { monitors } monitors { edges { node { id } } } }`),
			want: 4,
		},
		{
			name: "inline fragments do not add a level",
			doc:  parseDepthDocument(t, `{ stats { ... on Stats { monitors } } }`),
			want: 2,
		},
		{
			name: "the deepest operation wins",
			doc:  parseDepthDocument(t, `query Shallow { stats } query Deep { alerts { edges { node { id } } } }`),
			want: 4,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CalculateQueryDepth(tc.doc))
		})
	}
}
