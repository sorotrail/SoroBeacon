package archive

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// recordingArchiver captures what a Pruner writes without touching disk.
type recordingArchiver struct {
	keys   []string
	bodies []string
}

func (r *recordingArchiver) Archive(_ context.Context, key string, rd io.Reader) error {
	b, err := io.ReadAll(rd)
	if err != nil {
		return err
	}
	r.keys = append(r.keys, key)
	r.bodies = append(r.bodies, string(b))
	return nil
}

// TestNewPrunerEdgeCases pins the constructor's boundary behaviour: a nil
// backend is invalid and rejected, an empty batch is a no-op, and a single
// alert is the smallest batch that produces an archive object.
func TestNewPrunerEdgeCases(t *testing.T) {
	day := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name         string
		backend      Archiver
		alerts       []store.Alert
		wantErr      string
		wantArchives int
	}{
		{
			name:    "nil backend is rejected",
			wantErr: "archive: nil backend",
		},
		{
			name:    "empty batch never reaches the backend",
			backend: &recordingArchiver{},
		},
		{
			name:         "single alert is the smallest valid batch",
			backend:      &recordingArchiver{},
			alerts:       []store.Alert{{ID: 7, CreatedAt: day}},
			wantArchives: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			back, _ := tt.backend.(*recordingArchiver)

			pruner, err := NewPruner(tt.backend)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				assert.Nil(t, pruner, "a rejected backend must not yield a pruner")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, pruner)
			require.NotNil(t, back)

			require.NoError(t, pruner.Archive(context.Background(), tt.alerts))
			require.Len(t, back.keys, tt.wantArchives)
			if tt.wantArchives == 0 {
				return
			}
			assert.Equal(t, BatchKey(tt.alerts), back.keys[0])
			assert.Equal(t, 1, strings.Count(back.bodies[0], "\n"), "one NDJSON line per alert")
		})
	}
}
