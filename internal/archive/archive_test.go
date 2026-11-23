package archive

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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