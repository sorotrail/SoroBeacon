package api

import (
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestValidateDigest(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		window  int64
		wantErr bool
	}{
		{"off by default", store.DigestModeOff, 0, false},
		{"off ignores a window", store.DigestModeOff, 60, false},
		{"negative window rejected", store.DigestModeOff, -1, true},
		{"window mode needs a window", store.DigestModeWindow, 0, true},
		{"window mode with a window", store.DigestModeWindow, 300, false},
		{"unknown mode rejected", "hourly", 300, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateDigest(tt.mode, tt.window)
			if tt.wantErr && len(got) == 0 {
				t.Fatalf("validateDigest(%q, %d) accepted an invalid value", tt.mode, tt.window)
			}
			if !tt.wantErr && len(got) != 0 {
				t.Fatalf("validateDigest(%q, %d) = %v", tt.mode, tt.window, got)
			}
		})
	}
}
