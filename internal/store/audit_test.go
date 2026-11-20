package store

import "testing"

// ValidAuditOutcome gates the outcome filter on the audit log, so an
// unsupported value has to be rejected at the boundary instead of reaching
// the query and quietly matching nothing.
func TestValidAuditOutcome(t *testing.T) {
	tests := []struct {
		name    string
		outcome string
		want    bool
	}{
		{name: "succeeded", outcome: AuditOutcomeSucceeded, want: true},
		{name: "failed", outcome: AuditOutcomeFailed, want: true},
		{name: "empty", outcome: "", want: false},
		{name: "unknown value", outcome: "pending", want: false},
		{name: "wrong case", outcome: "Succeeded", want: false},
		{name: "surrounding whitespace", outcome: " succeeded", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidAuditOutcome(tc.outcome); got != tc.want {
				t.Errorf("ValidAuditOutcome(%q) = %v, want %v", tc.outcome, got, tc.want)
			}
		})
	}
}
