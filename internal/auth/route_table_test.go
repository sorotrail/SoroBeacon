package auth

import "testing"

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
