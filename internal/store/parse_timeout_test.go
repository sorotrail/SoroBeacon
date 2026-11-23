package store_test

import (
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTimeout(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"empty uses default", "", store.DefaultChannelTimeout, false},
		{"null uses default", "null", store.DefaultChannelTimeout, false},
		{"blank string uses default", `""`, store.DefaultChannelTimeout, false},
		{"integer seconds", "15", 15 * time.Second, false},
		{"minimum boundary", "1", store.MinChannelTimeout, false},
		{"duration string", `"30s"`, 30 * time.Second, false},
		{"numeric string", `"20"`, 20 * time.Second, false},

		{"zero seconds is too short", "0", 0, true},
		{"negative seconds", "-5", 0, true},
		{"above the ceiling", "100000", 0, true},
		{"unparseable string", `"soon"`, 0, true},
		{"wrong json type", "true", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.ParseTimeout([]byte(tt.raw))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
