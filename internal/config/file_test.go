package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadUsesConfigFileDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`DATABASE_URL: postgres://file.example/db
POLL_INTERVAL: 15s
HTTP_ADDR: ":9090"
LOG_LEVEL: warn
API_TOKEN: token-from-file`), 0o600))

	t.Setenv("CONFIG_FILE", path)
	t.Setenv("DATABASE_URL", "postgres://env.example/db")
	t.Setenv("POLL_INTERVAL", "30s")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("API_TOKEN", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://env.example/db", cfg.DatabaseURL)
	assert.Equal(t, 30*time.Second, cfg.PollInterval)
	assert.Equal(t, ":9090", cfg.HTTPAddr)
	assert.Equal(t, slog.LevelWarn, cfg.LogLevel)
	assert.Equal(t, []string{"token-from-file"}, cfg.APITokens)
}

func TestLoadFileAppliesValuesWhenEnvUnset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`DATABASE_URL: postgres://file.example/db
SOURCE_MODE: sorotrail
SOROTRAIL_URL: http://index.example
RATE_LIMIT_RPS: 12.5
RATE_LIMIT_BURST: 8
CORS_ALLOWED_ORIGINS:
  - https://a.example
  - https://b.example
`), 0o600))

	t.Setenv("CONFIG_FILE", path)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("SOURCE_MODE", "")
	t.Setenv("SOROTRAIL_URL", "")
	t.Setenv("RATE_LIMIT_RPS", "")
	t.Setenv("RATE_LIMIT_BURST", "")
	t.Setenv("CORS_ALLOWED_ORIGINS", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://file.example/db", cfg.DatabaseURL)
	assert.Equal(t, "sorotrail", cfg.SourceMode)
	assert.Equal(t, "http://index.example", cfg.SoroTrailURL)
	assert.Equal(t, 12.5, cfg.RateLimitRPS)
	assert.Equal(t, 8, cfg.RateLimitBurst)
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, cfg.CORSAllowedOrigins)
}

func TestLoadFileErrorsOnMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.yaml")
	require.NoError(t, os.WriteFile(path, []byte("DATABASE_URL: postgres://example/db\nPOLL_INTERVAL: [\n"), 0o600))

	t.Setenv("CONFIG_FILE", path)
	t.Setenv("DATABASE_URL", "")

	_, err := Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "CONFIG_FILE")
	assert.ErrorContains(t, err, path)
	assert.NotContains(t, err.Error(), "postgres://example/db")
}

func TestLoadFileFailsValidationLikeEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte("DATABASE_URL: not-a-url\nHTTP_ADDR: no-port\n"), 0o600))

	t.Setenv("CONFIG_FILE", path)
	_, err := Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "DATABASE_URL")
	assert.NotContains(t, err.Error(), "not-a-url")
	assert.NotContains(t, err.Error(), "no-port")
}
