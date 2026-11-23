package store_test

import (
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
)

// ValidAuditTargetType is the whitelist for the audit_log.target_type filter,
// so it must accept every known target and reject anything else.
func TestValidAuditTargetType(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"monitor constant", store.AuditTargetMonitor, true},
		{"rule constant", store.AuditTargetRule, true},
		{"channel constant", store.AuditTargetChannel, true},
		{"session constant", store.AuditTargetSession, true},
		{"workspace constant", store.AuditTargetWorkspace, true},
		{"dead letter literal", "dead_letter", true},
		{"alert partition literal", "alert_partition", true},
		{"retention policy literal", "retention_policy", true},

		{"empty", "", false},
		{"unknown target", "dashboard", false},
		{"wrong case", "Monitor", false},
		{"padded", " monitor ", false},
		{"hyphen instead of underscore", "dead-letter", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, store.ValidAuditTargetType(tt.in))
		})
	}
}
