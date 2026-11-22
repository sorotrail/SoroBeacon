package auth

import (
	"reflect"
	"testing"
)

func TestPatternMatches(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{
			name:    "parameter matches one segment",
			pattern: "/monitors/{id}",
			path:    "/monitors/7",
			want:    true,
		},
		{
			name:    "segment count mismatch",
			pattern: "/monitors/{id}",
			path:    "/monitors/7/rules",
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PatternMatches(tc.pattern, tc.path); got != tc.want {
				t.Fatalf("PatternMatches(%q, %q) = %t, want %t", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestRouteTablePatterns(t *testing.T) {
	t.Run("sorted copy of route keys", func(t *testing.T) {
		tbl := NewRouteTable(map[string]string{
			"/monitors/{id}": "detail",
			"/healthz":       "health",
			"/monitors":      "list",
		})
		want := []string{"/healthz", "/monitors", "/monitors/{id}"}
		got := tbl.Patterns()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Patterns() = %v, want %v", got, want)
		}
		got[0] = "/mutated"
		if again := tbl.Patterns(); !reflect.DeepEqual(again, want) {
			t.Fatalf("Patterns() after caller mutation = %v, want %v", again, want)
		}
	})

	t.Run("nil route table", func(t *testing.T) {
		var tbl *RouteTable[string]
		if got := tbl.Patterns(); got != nil {
			t.Fatalf("(*RouteTable).Patterns() = %v, want nil", got)
		}
	})
}
