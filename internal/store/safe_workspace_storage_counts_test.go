package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeUsageStorageCounts struct {
	store.Usage
	counts store.WorkspaceStorageCounts
	err    error
	panic  bool
}

func (f *fakeUsageStorageCounts) GetWorkspaceStorageCounts(_ context.Context, _ string) (store.WorkspaceStorageCounts, error) {
	if f.panic {
		panic("unimplemented")
	}
	return f.counts, f.err
}

func TestSafeGetWorkspaceStorageCounts(t *testing.T) {
	ctx := context.Background()
	wantCounts := store.WorkspaceStorageCounts{
		Monitors: 3,
		Rules:    7,
		Alerts:   12,
	}
	wantErr := errors.New("db unavailable")

	tests := []struct {
		name    string
		st      store.Usage
		want    store.WorkspaceStorageCounts
		wantErr error
	}{
		{
			name: "nil store returns zero counts",
			st:   nil,
			want: store.WorkspaceStorageCounts{},
		},
		{
			name: "panicking partial fake recovers to zero counts",
			st:   &fakeUsageStorageCounts{panic: true},
			want: store.WorkspaceStorageCounts{},
		},
		{
			name: "valid store returns counts",
			st:   &fakeUsageStorageCounts{counts: wantCounts},
			want: wantCounts,
		},
		{
			name:    "store error is propagated",
			st:      &fakeUsageStorageCounts{err: wantErr},
			wantErr: wantErr,
		},
	}

	_ = time.Second
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.SafeGetWorkspaceStorageCounts(ctx, tt.st, "ws-1")
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
