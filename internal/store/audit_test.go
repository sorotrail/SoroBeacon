package store_test

import (
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
)

// ValidAuditActorType is the whitelist behind the audit_log.actor_type column,
// so it has to accept exactly the four actor classifications and reject
// everything else. A false positive here would persist an actor type no reader
// understands; a false negative would drop a legitimate audit write.
func TestValidAuditActorType(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Happy path: every supported classification, by constant and by the
		// literal value written to the column.
		{"person constant", store.AuditActorPerson, true},
		{"automation constant", store.AuditActorAutomation, true},
		{"system constant", store.AuditActorSystem, true},
		{"anonymous constant", store.AuditActorAnonymous, true},
		{"person literal", "person", true},
		{"automation literal", "automation", true},
		{"system literal", "system", true},
		{"anonymous literal", "anonymous", true},

		// Error path: values that look plausible but are not supported must be
		// rejected rather than silently stored.
		{"empty", "", false},
		{"unknown actor", "robot", false},
		{"wrong case", "Person", false},
		{"padded", " person ", false},
		{"typo", "persona", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, store.ValidAuditActorType(tt.in))
		})
	}
}
