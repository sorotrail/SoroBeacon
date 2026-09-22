package config

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:secret@db.example:5432/sorobeacon")
	t.Setenv("RPC_URL", "")
	t.Setenv("NETWORK", "")
	t.Setenv("NETWORK_PASSPHRASE", "")
	t.Setenv("POLL_INTERVAL", "5s")
	t.Setenv("HTTP_ADDR", ":8080")
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("SOURCE_MODE", "rpc")
	t.Setenv("SOROTRAIL_URL", "")
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	t.Setenv("HTTP_MAX_BODY_BYTES", "")
	t.Setenv("READYZ_LAG_THRESHOLD", "")
	t.Setenv("ALERT_RETENTION", "")
}

func TestReloadableList(t *testing.T) {
	assert.Equal(t, []string{"log_level", "poll_interval"}, Reloadable)
}

func TestReloadAppliesLogLevelAndPollInterval(t *testing.T) {
	baseEnv(t)
	current, err := Load()
	require.NoError(t, err)
	assert.Equal(t, slog.LevelInfo, current.LogLevel)
	assert.Equal(t, 5*time.Second, current.PollInterval)

	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("POLL_INTERVAL", "15s")

	next, result, err := Reload(current)
	require.NoError(t, err)
	assert.Equal(t, slog.LevelDebug, next.LogLevel)
	assert.Equal(t, 15*time.Second, next.PollInterval)
	assert.Equal(t, current.DatabaseURL, next.DatabaseURL)
	assert.Equal(t, current.HTTPAddr, next.HTTPAddr)
	require.Len(t, result.Applied, 2)
	assert.Equal(t, "log_level", result.Applied[0].Name)
	assert.Equal(t, "info", result.Applied[0].From)
	assert.Equal(t, "debug", result.Applied[0].To)
	assert.Equal(t, "poll_interval", result.Applied[1].Name)
	assert.Equal(t, "5s", result.Applied[1].From)
	assert.Equal(t, "15s", result.Applied[1].To)
	assert.Empty(t, result.Skipped)
}

func TestReloadRejectsInvalidKeepsPrevious(t *testing.T) {
	baseEnv(t)
	current, err := Load()
	require.NoError(t, err)

	t.Setenv("LOG_LEVEL", "not-a-level")
	t.Setenv("POLL_INTERVAL", "30s")

	next, result, err := Reload(current)
	require.Error(t, err)
	assert.ErrorContains(t, err, "LOG_LEVEL")
	assert.Equal(t, current, next)
	assert.Empty(t, result.Applied)
	assert.Empty(t, result.Skipped)

	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("POLL_INTERVAL", "500ms") // below the 1s minimum
	next, result, err = Reload(current)
	require.Error(t, err)
	assert.ErrorContains(t, err, "1s")
	assert.Equal(t, current, next)
	assert.Empty(t, result.Applied)
}

func TestReloadSkipsNonReloadable(t *testing.T) {
	baseEnv(t)
	current, err := Load()
	require.NoError(t, err)

	t.Setenv("DATABASE_URL", "postgres://other:password@elsewhere:5432/sorobeacon")
	t.Setenv("HTTP_ADDR", ":9999")
	t.Setenv("SOURCE_MODE", "sorotrail")
	t.Setenv("SOROTRAIL_URL", "http://indexer.example")
	t.Setenv("LOG_LEVEL", "warn")

	next, result, err := Reload(current)
	require.NoError(t, err)
	assert.Equal(t, slog.LevelWarn, next.LogLevel)
	assert.Equal(t, current.DatabaseURL, next.DatabaseURL, "DATABASE_URL must not change without a restart")
	assert.Equal(t, current.HTTPAddr, next.HTTPAddr)
	assert.Equal(t, current.SourceMode, next.SourceMode)
	assert.Equal(t, current.SoroTrailURL, next.SoroTrailURL)

	require.Len(t, result.Applied, 1)
	assert.Equal(t, "log_level", result.Applied[0].Name)
	assert.Equal(t, "info", result.Applied[0].From)
	assert.Equal(t, "warn", result.Applied[0].To)

	skipped := map[string]Skip{}
	for _, s := range result.Skipped {
		skipped[s.Name] = s
		assert.Equal(t, skipNotReloadable, s.Reason)
	}
	require.Contains(t, skipped, "database_url")
	assert.NotContains(t, skipped["database_url"].From, "secret")
	assert.NotContains(t, skipped["database_url"].To, "password")
	assert.Contains(t, skipped["database_url"].From, "db.example")
	assert.Contains(t, skipped["database_url"].To, "elsewhere")
	require.Contains(t, skipped, "http_addr")
	assert.Equal(t, ":8080", skipped["http_addr"].From)
	assert.Equal(t, ":9999", skipped["http_addr"].To)
	require.Contains(t, skipped, "source_mode")
	assert.Equal(t, "rpc", skipped["source_mode"].From)
	assert.Equal(t, "sorotrail", skipped["source_mode"].To)
}

func TestReloadNoop(t *testing.T) {
	baseEnv(t)
	current, err := Load()
	require.NoError(t, err)

	next, result, err := Reload(current)
	require.NoError(t, err)
	assert.Equal(t, current, next)
	assert.Empty(t, result.Applied)
	assert.Empty(t, result.Skipped)
}

func TestReloadConcurrent(t *testing.T) {
	baseEnv(t)
	current, err := Load()
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := Reload(current)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
}
