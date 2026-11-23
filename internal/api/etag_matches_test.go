package api

import "testing"

func TestETagMatches(t *testing.T) {
	const etag = `"abc123"`
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "exact strong match", header: `"abc123"`, want: true},
		{name: "wildcard matches anything", header: `*`, want: true},
		{name: "weak validator matches strong tag", header: `W/"abc123"`, want: true},
		{name: "match inside a list with whitespace", header: ` "zzz" ,  "abc123" `, want: true},
		{name: "weak match inside a list", header: `"zzz", W/"abc123"`, want: true},
		{name: "empty header does not match", header: ``, want: false},
		{name: "different tag does not match", header: `"other"`, want: false},
		{name: "unquoted value does not match quoted tag", header: `abc123`, want: false},
		{name: "prefix of tag does not match", header: `"abc"`, want: false},
		{name: "list with no matching entry", header: `"a", "b", W/"c"`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ETagMatches(tt.header, etag); got != tt.want {
				t.Fatalf("ETagMatches(%q, %q) = %v, want %v", tt.header, etag, got, tt.want)
			}
		})
	}
}
