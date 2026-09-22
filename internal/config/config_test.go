package config

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	// Pin every documented default. Clear optional vars so a leaked
	// environment cannot masquerade as the unset path.
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("RPC_URL", "")
	t.Setenv("NETWORK", "")
	t.Setenv("NETWORK_PASSPHRASE", "")
	t.Setenv("POLL_INTERVAL", "")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("SOURCE_MODE", "")
	t.Setenv("SOROTRAIL_URL", "")
	t.Setenv("CORS_ALLOWED_ORIGINS", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://x", cfg.DatabaseURL)
	assert.Equal(t, "testnet", cfg.Network.Name)
	assert.Equal(t, DefaultRPCURL, cfg.RPCURL)
	assert.Equal(t, DefaultRPCURL, cfg.Network.RPCURL)
	assert.Equal(t, DefaultPollInterval, cfg.PollInterval)
	assert.Equal(t, DefaultHTTPAddr, cfg.HTTPAddr)
	assert.Equal(t, DefaultHTTPMaxBodyBytes, cfg.HTTPMaxBodyBytes)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
	assert.Equal(t, uint32(0), cfg.ReadyzLagThreshold)
	assert.Equal(t, 0.0, cfg.RateLimitRPS)
	assert.Equal(t, 0, cfg.RateLimitBurst)
	assert.False(t, cfg.RateLimitTrustForwarded)
	assert.Equal(t, "rpc", cfg.SourceMode)
	assert.Empty(t, cfg.SoroTrailURL)
	assert.Empty(t, cfg.CORSAllowedOrigins)
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	_, err := Load()
	assert.ErrorContains(t, err, "DATABASE_URL")
}

func TestLoadRequiresSoroTrailURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("SOURCE_MODE", "sorotrail")
	t.Setenv("SOROTRAIL_URL", "")

	_, err := Load()
	assert.ErrorContains(t, err, "SOROTRAIL_URL")
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://custom")
	t.Setenv("RPC_URL", "https://mainnet.example")
	t.Setenv("POLL_INTERVAL", "30s")
	t.Setenv("HTTP_ADDR", ":9999")
	t.Setenv("HTTP_MAX_BODY_BYTES", "4096")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SOURCE_MODE", "sorotrail")
	t.Setenv("SOROTRAIL_URL", "http://indexer.example")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://ops.example, https://other.example")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://custom", cfg.DatabaseURL)
	assert.Equal(t, "https://mainnet.example", cfg.RPCURL)
	assert.Equal(t, 30*time.Second, cfg.PollInterval)
	assert.Equal(t, ":9999", cfg.HTTPAddr)
	assert.Equal(t, int64(4096), cfg.HTTPMaxBodyBytes)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
	assert.Equal(t, "sorotrail", cfg.SourceMode)
	assert.Equal(t, "http://indexer.example", cfg.SoroTrailURL)
	assert.Equal(t, []string{"https://ops.example", "https://other.example"}, cfg.CORSAllowedOrigins)
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

func TestLoadHTTPAddr(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{name: "all interfaces", addr: ":8080"},
		{name: "loopback", addr: "127.0.0.1:8080"},
		{name: "wildcard ipv4", addr: "0.0.0.0:9090"},
		{name: "missing colon", addr: "8080", wantErr: true},
		{name: "non-numeric port", addr: ":abc", wantErr: true},
		{name: "port out of range", addr: ":99999", wantErr: true},
		{name: "empty", addr: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHTTPAddr(tt.addr)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, "HTTP_ADDR")
			assert.ErrorContains(t, err, tt.addr)
			assert.ErrorContains(t, err, "host:port")
		})
	}
}

func TestLoadAcceptsValidHTTPAddr(t *testing.T) {
	for _, addr := range []string{":8080", "127.0.0.1:8080", "0.0.0.0:9090"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv("HTTP_ADDR", addr)

			cfg, err := Load()

			require.NoError(t, err)
			assert.Equal(t, addr, cfg.HTTPAddr)
		})
	}
}

func TestLoadRejectsInvalidHTTPAddr(t *testing.T) {
	for _, addr := range []string{"8080", ":abc", ":99999"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv("HTTP_ADDR", addr)

			_, err := Load()

			assert.ErrorContains(t, err, "HTTP_ADDR")
			assert.ErrorContains(t, err, addr)
			assert.ErrorContains(t, err, "host:port")
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

	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("SOURCE_MODE", "kafka")
	_, err = Load()
	assert.ErrorContains(t, err, "SOURCE_MODE")

	t.Setenv("SOURCE_MODE", "rpc")
	t.Setenv("HTTP_MAX_BODY_BYTES", "nope")
	_, err = Load()
	assert.ErrorContains(t, err, "HTTP_MAX_BODY_BYTES")

	t.Setenv("HTTP_MAX_BODY_BYTES", "0")
	_, err = Load()
	assert.ErrorContains(t, err, "HTTP_MAX_BODY_BYTES")
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
