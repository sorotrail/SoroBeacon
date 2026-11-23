package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSQLiteReadOnly(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, Migrate(url))

	st, err := NewSQLiteReadOnly(ctx, url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	require.NoError(t, st.Ping(ctx))

	// Reads work against the migrated schema.
	var n int
	require.NoError(t, st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitors`).Scan(&n))
	assert.Zero(t, n)

	// Writes are refused: the handle is opened mode=ro with query_only.
	_, err = st.db.ExecContext(ctx, `CREATE TABLE should_not_exist (id INTEGER)`)
	assert.Error(t, err)
}

func TestNewSQLiteReadOnlyErrors(t *testing.T) {
	missing := "sqlite://" + filepath.Join(t.TempDir(), "absent", "sorobeacon.db")
	tests := []struct {
		name    string
		url     string
		wantErr string
	}{
		{name: "non-sqlite scheme", url: "postgres://localhost/sorobeacon", wantErr: "is not sqlite"},
		{name: "missing file path", url: "sqlite://", wantErr: "missing a database file path"},
		{name: "database file does not exist", url: missing, wantErr: "ping sqlite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := NewSQLiteReadOnly(context.Background(), tt.url)
			require.Error(t, err)
			assert.Nil(t, st)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPriorityNormalized(t *testing.T) {
	tests := []struct {
		name string
		in   Priority
		want Priority
	}{
		{name: "empty maps to normal", in: "", want: PriorityNormal},
		{name: "low is kept", in: PriorityLow, want: PriorityLow},
		{name: "normal is kept", in: PriorityNormal, want: PriorityNormal},
		{name: "high is kept", in: PriorityHigh, want: PriorityHigh},
		{name: "unknown value passes through unchanged", in: Priority("urgent"), want: Priority("urgent")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.in.Normalized())
		})
	}
}
