package archive

import (
	"context"
	"time"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// TestNewPrunerEdgeCases covers edge cases for NewPruner:
// - nil backend should return an error
// - a valid backend should succeed
func TestNewPrunerEdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(t *testing.T) error
		wantErr   bool
	}{
		{
			name: "nil backend returns error",
			setupFunc: func(t *testing.T) error {
				// nil backend - should error
				var back Archiver = nil //nolint:staticcheck // we want to test nil
				_, err := NewPruner(back)
				assert.Error(t, err, "nil backend must return an error")
				return err
			},
			wantErr: true,
		},
		{
			name: "valid backend succeeds",
			setupFunc: func(t *testing.T) error {
				dir := t.TempDir()
				back, err := NewDir(dir)
				require.NoError(t, err, "NewDir must succeed")
				_, err = NewPruner(back)
				require.NoError(t, err, "NewPruner with valid backend must succeed")
				return nil
			},
			wantErr: false,
		},
		{
			name: "empty alerts is no-op",
			setupFunc: func(t *testing.T) error {
				back, err := NewDir(t.TempDir())
				require.NoError(t, err, "NewDir must succeed")
				p, err := NewPruner(back)
				require.NoError(t, err, "NewPruner must succeed")
				// empty alerts should be a no-op
				err = p.Archive(context.Background(), []store.Alert{})
				assert.NoError(t, err, "empty alerts must succeed as no-op")
				return nil
			},
			wantErr: false,
		},
		{
			name: "single alert boundary",
			setupFunc: func(t *testing.T) error {
				back, err := NewDir(t.TempDir())
				require.NoError(t, err, "NewDir must succeed")
				p, err := NewPruner(back)
				require.NoError(t, err, "NewPruner must succeed")
				// single alert - boundary value
				singleAlert := store.Alert{
					ID:        42,
					MonitorID: 1,
					RuleID:    1,
					EventID:   "test-event",
					CreatedAt: time.Now(),
					Payload:   []byte(`{"key":"value"}`),
				}
				err = p.Archive(context.Background(), []store.Alert{singleAlert})
				assert.NoError(t, err, "single alert must succeed")
				// verify the key is deterministic
				key := BatchKey([]store.Alert{singleAlert})
				assert.Contains(t, key, "alerts/")
				assert.Contains(t, key, ".ndjson")
				return nil
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.setupFunc(t)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}