package config

import (
	"log/slog"
	"strings"
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
	assert.Equal(t, uint32(0), cfg.ReadyzLagThreshold)
	assert.Equal(t, 0.0, cfg.RateLimitRPS)
	assert.Equal(t, 0, cfg.RateLimitBurst)
	assert.False(t, cfg.RateLimitTrustForwarded)
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

func TestLoadReadyzLagThreshold(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("READYZ_LAG_THRESHOLD", "50")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, uint32(50), cfg.ReadyzLagThreshold)
}

func TestLoadRejectsInvalidReadyzLagThreshold(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("READYZ_LAG_THRESHOLD", "nope")

	_, err := Load()
	assert.ErrorContains(t, err, "READYZ_LAG_THRESHOLD")
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

func TestLoadRateLimit(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("RATE_LIMIT_RPS", "10")
	t.Setenv("RATE_LIMIT_TRUST_FORWARDED", "true")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 10.0, cfg.RateLimitRPS)
	assert.Equal(t, 10, cfg.RateLimitBurst)
	assert.True(t, cfg.RateLimitTrustForwarded)

	t.Setenv("RATE_LIMIT_BURST", "25")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, 25, cfg.RateLimitBurst)
}

func TestLoadRejectsBadRateLimit(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("RATE_LIMIT_RPS", "nope")
	_, err := Load()
	assert.ErrorContains(t, err, "RATE_LIMIT_RPS")

	t.Setenv("RATE_LIMIT_RPS", "-1")
	_, err = Load()
	assert.ErrorContains(t, err, "RATE_LIMIT_RPS")

	t.Setenv("RATE_LIMIT_RPS", "1")
	t.Setenv("RATE_LIMIT_BURST", "-2")
	_, err = Load()
	assert.ErrorContains(t, err, "RATE_LIMIT_BURST")

	t.Setenv("RATE_LIMIT_BURST", "1")
	t.Setenv("RATE_LIMIT_TRUST_FORWARDED", "maybe")
	_, err = Load()
	assert.ErrorContains(t, err, "RATE_LIMIT_TRUST_FORWARDED")
}

func TestLogAttrsRedactsDatabasePassword(t *testing.T) {
	cfg := Config{
		Network:            Network{Name: "testnet", Passphrase: "Public Global Stellar Network ; September 2015"},
		RPCURL:             "https://soroban-testnet.stellar.org",
		DatabaseURL:        "postgres://humaki:s3cret-pass@db.example:5432/beacon?sslmode=disable",
		PollInterval:       DefaultPollInterval,
		SourceMode:         "rpc",
		SoroTrailURL:       "",
		CORSAllowedOrigins: []string{"https://app.example"},
		HTTPAddr:           ":8080",
		LogLevel:           slog.LevelInfo,
	}

	attrs := cfg.LogAttrs()
	got := map[string]string{}
	var dump strings.Builder
	for _, a := range attrs {
		val := a.Value.String()
		got[a.Key] = val
		dump.WriteString(a.Key)
		dump.WriteByte('=')
		dump.WriteString(val)
		dump.WriteByte('\n')
	}
	blob := dump.String()

	assert.Equal(t, "postgres://db.example:5432/beacon", got["database_url"])
	assert.Equal(t, ":8080", got["http_addr"])
	assert.Equal(t, "rpc", got["source_mode"])
	assert.Equal(t, "5s", got["poll_interval"])
	assert.Equal(t, "info", got["log_level"])
	assert.Equal(t, "testnet", got["network"])
	assert.Equal(t, "https://soroban-testnet.stellar.org", got["rpc_url"])
	assert.Equal(t, "", got["sorotrail_url"])
	assert.Equal(t, "https://app.example", got["cors_allowed_origins"])

	assert.NotContains(t, blob, "s3cret-pass")
	assert.NotContains(t, blob, "humaki")
	assert.NotContains(t, blob, "sslmode")
	assert.NotContains(t, blob, "Public Global Stellar Network")
}

func TestRedactDatabaseURLUnparseable(t *testing.T) {
	assert.Equal(t, "", redactDatabaseURL(""))
	assert.Equal(t, redacted, redactDatabaseURL("not a url"))
	assert.Equal(t, redacted, redactDatabaseURL("http://["))
}

func TestLogAttrsOptInDoesNotDumpWholeStruct(t *testing.T) {
	cfg := Config{
		DatabaseURL: "postgres://user:pw@host/db",
		HTTPAddr:    ":8080",
		SourceMode:  "rpc",
		LogLevel:    slog.LevelWarn,
		Network:     Network{Name: "mainnet"},
	}
	keys := make([]string, 0, len(cfg.LogAttrs()))
	for _, a := range cfg.LogAttrs() {
		keys = append(keys, a.Key)
	}
	assert.Equal(t, []string{
		"database_url",
		"http_addr",
		"source_mode",
		"poll_interval",
		"log_level",
		"network",
		"rpc_url",
		"sorotrail_url",
		"cors_allowed_origins",
	}, keys)
}
