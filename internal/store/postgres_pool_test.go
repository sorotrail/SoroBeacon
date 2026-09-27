package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const poolTestURL = "postgres://sorobeacon:sorobeacon@localhost:5432/sorobeacon?sslmode=disable"

func TestBuildPoolConfigLeavesDriverDefaultsWhenUnset(t *testing.T) {
	want, err := pgxpool.ParseConfig(poolTestURL)
	require.NoError(t, err)

	got, err := buildPoolConfig(poolTestURL, PoolSettings{})
	require.NoError(t, err)

	assert.Equal(t, want.MaxConns, got.MaxConns)
	assert.Equal(t, want.MinConns, got.MinConns)
	assert.Equal(t, want.MaxConnLifetime, got.MaxConnLifetime)
	assert.Equal(t, want.MaxConnIdleTime, got.MaxConnIdleTime)
}

func TestBuildPoolConfigAppliesSettings(t *testing.T) {
	settings := PoolSettings{
		MaxConns:        7,
		MinConns:        3,
		MaxConnLifetime: time.Hour,
		MaxConnIdleTime: 5 * time.Minute,
	}

	got, err := buildPoolConfig(poolTestURL, settings)
	require.NoError(t, err)

	assert.Equal(t, int32(7), got.MaxConns)
	assert.Equal(t, int32(3), got.MinConns)
	assert.Equal(t, time.Hour, got.MaxConnLifetime)
	assert.Equal(t, 5*time.Minute, got.MaxConnIdleTime)
}
