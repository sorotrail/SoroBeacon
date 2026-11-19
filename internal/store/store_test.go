package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// NormalizeAlertSearch trims whitespace; a blank or whitespace-only term
// means "no filter" rather than "match nothing".
func TestNormalizeAlertSearch(t *testing.T) {
	tests := []struct {
		name string
		q    string
		want string
	}{
		{
			name: "happy path: plain term is returned unchanged",
			q:    "transfer",
			want: "transfer",
		},
		{
			name: "happy path: surrounding spaces are trimmed",
			q:    "  transfer  ",
			want: "transfer",
		},
		{
			name: "happy path: tabs and newlines are trimmed",
			q:    "\ttransfer\n",
			want: "transfer",
		},
		{
			name: "error path: empty term means no filter",
			q:    "",
			want: "",
		},
		{
			name: "error path: whitespace-only term means no filter",
			q:    "   ",
			want: "",
		},
		{
			name: "error path: mixed whitespace-only term means no filter",
			q:    " \t\n ",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeAlertSearch(tt.q)
			assert.Equal(t, tt.want, got)
			assert.False(t, strings.HasPrefix(got, " "), "normalized term must never have leading whitespace")
			assert.False(t, strings.HasSuffix(got, " "), "normalized term must never have trailing whitespace")
		})
	}
}
