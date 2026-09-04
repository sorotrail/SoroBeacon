package config

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, DefaultRPCURL, cfg.RPCURL)
	assert.Equal(t, DefaultPollInterval, cfg.PollInterval)
	assert.Equal(t, DefaultHTTPAddr, cfg.HTTPAddr)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	_, err := Load()
	assert.ErrorContains(t, err, "DATABASE_URL")
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("RPC_URL", "https://mainnet.example")
	t.Setenv("POLL_INTERVAL", "30s")
	t.Setenv("HTTP_ADDR", ":9999")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "https://mainnet.example", cfg.RPCURL)
	assert.Equal(t, 30*time.Second, cfg.PollInterval)
	assert.Equal(t, ":9999", cfg.HTTPAddr)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
}

func TestLoadAcceptsHTTPAndHTTPSRPCURLs(t *testing.T) {
	for _, rpcURL := range []string{
		"http://localhost:8000",
		"https://mainnet.example",
	} {
		t.Run(rpcURL, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv("RPC_URL", rpcURL)

			cfg, err := Load()

			require.NoError(t, err)
			assert.Equal(t, rpcURL, cfg.RPCURL)
		})
	}
}

func TestLoadRejectsInvalidRPCURL(t *testing.T) {
	tests := []struct {
		name   string
		rpcURL string
	}{
		{
			name:   "unsupported scheme",
			rpcURL: "ftp://example.com",
		},
		{
			name:   "missing scheme",
			rpcURL: "example.com",
		},
		{
			name:   "missing host",
			rpcURL: "https:",
		},
		{
			name:   "misspelled scheme",
			rpcURL: "htps://example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv("RPC_URL", tt.rpcURL)

			_, err := Load()

			assert.ErrorContains(t, err, "RPC_URL")
			assert.ErrorContains(t, err, "absolute http or https URL")
		})
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	t.Setenv("POLL_INTERVAL", "nope")
	_, err := Load()
	assert.ErrorContains(t, err, "POLL_INTERVAL")

	t.Setenv("POLL_INTERVAL", "100ms")
	_, err = Load()
	assert.ErrorContains(t, err, "1s minimum")

	t.Setenv("POLL_INTERVAL", "5s")
	t.Setenv("LOG_LEVEL", "loud")
	_, err = Load()
	assert.ErrorContains(t, err, "LOG_LEVEL")
}
