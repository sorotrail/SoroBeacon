package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Table-driven tests for ValidAuditSurface.
// Covers the happy path (valid surfaces) and error path (invalid surfaces).
func TestValidAuditSurface(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect bool
	}{
		{
			name:   "monitor surface is valid",
			input:  "monitor",
			expect: true,
		},
		{
			name:   "rule surface is valid",
			input:  "rule",
			expect: true,
		},
		{
			name:   "channel surface is valid",
			input:  "channel",
			expect: true,
		},
		{
			name:   "empty surface is invalid",
			input:  "",
			expect: false,
		},
		{
			name:   "unknown surface is invalid",
			input:  "unknown",
			expect: false,
		},
		{
			name:   "nil-like surface is invalid",
			input:  "  ",
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidAuditSurface(tt.input)
			assert.Equal(t, tt.expect, got, "ValidAuditSurface(%q)", tt.input)
		})
	}
}