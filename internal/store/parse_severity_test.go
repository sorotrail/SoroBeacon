package store_test

import (
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
)

func TestParseSeverity(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   store.Severity
		wantOK bool
	}{
		{"empty defaults to warning", "", store.SeverityWarning, true},
		{"info", "info", store.SeverityInfo, true},
		{"warning", "warning", store.SeverityWarning, true},
		{"critical", "critical", store.SeverityCritical, true},

		{"unknown value", "fatal", "", false},
		{"wrong case", "Critical", "", false},
		{"padded", " info ", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := store.ParseSeverity(tt.in)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
