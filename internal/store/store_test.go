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

// ValidMaintenanceScope is the validation guard behind maintenance window
// scopes, accepting only global, monitor, and contract scopes.
func TestValidMaintenanceScope(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Happy path: every accepted scope value by constant and literal.
		{name: "happy path: global constant", in: MaintenanceScopeGlobal, want: true},
		{name: "happy path: monitor constant", in: MaintenanceScopeMonitor, want: true},
		{name: "happy path: contract constant", in: MaintenanceScopeContract, want: true},
		{name: "happy path: global literal", in: "global", want: true},
		{name: "happy path: monitor literal", in: "monitor", want: true},
		{name: "happy path: contract literal", in: "contract", want: true},

		// Error/invalid paths: empty, unknown values, wrong case, and padded whitespace.
		{name: "error path: empty scope", in: "", want: false},
		{name: "error path: unknown scope", in: "workspace", want: false},
		{name: "error path: unknown scope all", in: "all", want: false},
		{name: "error path: uppercase global", in: "GLOBAL", want: false},
		{name: "error path: title case monitor", in: "Monitor", want: false},
		{name: "error path: title case contract", in: "Contract", want: false},
		{name: "error path: leading and trailing whitespace", in: " global ", want: false},
		{name: "error path: whitespace-padded monitor", in: " monitor", want: false},
		{name: "error path: tab and newline padded contract", in: "\tcontract\n", want: false},
		{name: "error path: whitespace only", in: "   ", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ValidMaintenanceScope(tt.in))
		})
	}
}
