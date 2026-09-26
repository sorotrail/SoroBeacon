package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
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
	t.Setenv("RPC_URLS", "")
	t.Setenv("NETWORK", "")
	t.Setenv("NETWORK_PASSPHRASE", "")
	t.Setenv("POLL_INTERVAL", "")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("SOURCE_MODE", "")
	t.Setenv("SOROTRAIL_URL", "")
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	t.Setenv("CONFIG_ENCRYPTION_KEY", "")
	t.Setenv("API_TOKEN", "")

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
	assert.Zero(t, cfg.DatabaseMaxConns)
	assert.Zero(t, cfg.DatabaseMinConns)
	assert.Zero(t, cfg.DatabaseMaxConnLifetime)
	assert.Zero(t, cfg.DatabaseMaxConnIdleTime)
	assert.Equal(t, time.Duration(0), cfg.AlertRetention)
	assert.Equal(t, DefaultMonitorSilentAfter, cfg.MonitorSilentAfter)
	assert.Nil(t, cfg.ConfigEncryptionKey)
	assert.Empty(t, cfg.APITokens)
	// Tracing defaults to entirely off.
	assert.Empty(t, cfg.OTLP.Endpoint)
	assert.Empty(t, cfg.OTLP.ServiceName)
	assert.Equal(t, DefaultOTLPSampleRate, cfg.OTLP.SampleRate)
	assert.False(t, cfg.OTLP.Enabled())
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

func TestLoadOTLPOffByDefault(t *testing.T) {
	// Clear optional vars so a leaked environment cannot turn tracing on.
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("OTLP_ENDPOINT", "")
	t.Setenv("OTLP_SERVICE_NAME", "")
	t.Setenv("OTLP_SAMPLE_RATE", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.OTLP.Endpoint, "tracing must be off unless OTLP_ENDPOINT is set")
	assert.False(t, cfg.OTLP.Enabled())
}

func TestLoadOTLPConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("OTLP_ENDPOINT", "http://localhost:4318")
	t.Setenv("OTLP_SERVICE_NAME", "sorobeacon-staging")
	t.Setenv("OTLP_SAMPLE_RATE", "0.25")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:4318", cfg.OTLP.Endpoint)
	assert.True(t, cfg.OTLP.Enabled())
	assert.Equal(t, "sorobeacon-staging", cfg.OTLP.ServiceName)
	assert.Equal(t, 0.25, cfg.OTLP.SampleRate)
}

func TestLoadOTLPEndpointValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, bad := range []string{"localhost:4318", "ftp://collector", "http://"} {
		t.Setenv("OTLP_ENDPOINT", bad)
		_, err := Load()
		assert.ErrorContains(t, err, "OTLP_ENDPOINT", "endpoint %q must be rejected", bad)
	}
}

func TestLoadOTLPSampleRateValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("OTLP_ENDPOINT", "http://localhost:4318")
	for _, bad := range []string{"-0.1", "1.1", "abc", "NaN", "Inf"} {
		t.Setenv("OTLP_SAMPLE_RATE", bad)
		_, err := Load()
		assert.ErrorContains(t, err, "OTLP_SAMPLE_RATE", "rate %q must be rejected", bad)
	}

	// The bounds themselves are valid.
	for _, good := range []string{"0", "1"} {
		t.Setenv("OTLP_SAMPLE_RATE", good)
		_, err := Load()
		assert.NoError(t, err, "rate %q must be accepted", good)
	}
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

func TestLoadMonitorSilentAfter(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MONITOR_SILENT_AFTER", "48h")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 48*time.Hour, cfg.MonitorSilentAfter)
}

func TestLoadRejectsInvalidMonitorSilentAfter(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MONITOR_SILENT_AFTER", "nope")

	_, err := Load()
	assert.ErrorContains(t, err, "MONITOR_SILENT_AFTER")
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
			t.Setenv("RPC_URLS", "")

			cfg, err := Load()

			require.NoError(t, err)
			assert.Equal(t, rpcURL, cfg.RPCURL)
			// A lone RPC_URL is the single-entry case of the same list, so
			// downstream code always has a failover set to work with.
			assert.Equal(t, []string{rpcURL}, cfg.RPCURLs)
		})
	}
}

func TestLoadRPCURLs(t *testing.T) {
	t.Run("RPC_URLS takes priority over RPC_URL", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://x")
		t.Setenv("RPC_URL", "https://ignored.example")
		t.Setenv("RPC_URLS", "https://primary.example, https://fallback.example")

		cfg, err := Load()

		require.NoError(t, err)
		assert.Equal(t, "https://primary.example", cfg.RPCURL)
		assert.Equal(t, []string{"https://primary.example", "https://fallback.example"}, cfg.RPCURLs)
	})

	t.Run("RPC_URLS alone satisfies a custom network", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://x")
		t.Setenv("NETWORK", "custom")
		t.Setenv("NETWORK_PASSPHRASE", "Standalone Network ; September 2026")
		t.Setenv("RPC_URL", "")
		t.Setenv("RPC_URLS", "https://only.example")

		cfg, err := Load()

		require.NoError(t, err)
		assert.Equal(t, []string{"https://only.example"}, cfg.RPCURLs)
	})

	t.Run("an invalid entry fails at load", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://x")
		t.Setenv("RPC_URL", "")
		t.Setenv("RPC_URLS", "https://primary.example,not-a-url")

		_, err := Load()

		require.Error(t, err)
		assert.ErrorContains(t, err, "RPC_URLS")
		assert.ErrorContains(t, err, "not-a-url")
	})
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
			t.Setenv("RPC_URLS", "")

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

	t.Setenv("HTTP_MAX_BODY_BYTES", "1048576")
	t.Setenv("ALERT_RETENTION", "nope")
	_, err = Load()
	assert.ErrorContains(t, err, "ALERT_RETENTION")
}

func TestLoadAlertRetention(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("ALERT_RETENTION", "90d")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 90*24*time.Hour, cfg.AlertRetention)
}

func TestParseRetention(t *testing.T) {
	d, err := ParseRetention("")
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), d)

	d, err = ParseRetention("90d")
	require.NoError(t, err)
	assert.Equal(t, 90*24*time.Hour, d)

	d, err = ParseRetention("24h")
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, d)

	_, err = ParseRetention("0")
	assert.Error(t, err)
	_, err = ParseRetention("-1h")
	assert.Error(t, err)
	_, err = ParseRetention("0d")
	assert.Error(t, err)
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
		"replica_database_url",
		"http_addr",
		"source_mode",
		"poll_interval",
		"log_level",
		"network",
		"rpc_url",
		"rpc_endpoint_count",
		"sorotrail_url",
		"cors_allowed_origins",
		"config_encryption_enabled",
		"secrets_provider",
		"secrets_cache_ttl",
		"vault_token_configured",
		"reorg_tracking_window",
		"reorg_confirmation_depth",
		"api_token_count",
		"otlp_tracing_enabled",
		"otlp_service_name",
		"otlp_sample_rate",
	}, keys)
}

func TestLoadAPITokens(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	t.Setenv("API_TOKEN", "first")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"first"}, cfg.APITokens)

	// Two tokens at once is the rotation window: the new one is accepted
	// before the old one is dropped.
	t.Setenv("API_TOKEN", "old-token, new-token")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"old-token", "new-token"}, cfg.APITokens)
}

func TestLoadRejectsEmptyAPITokenList(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	// A value that is present but unusable must not fall back to open
	// access: the operator plainly meant to require a token.
	for _, raw := range []string{",", "   ", ",,"} {
		t.Setenv("API_TOKEN", raw)
		_, err := Load()
		assert.ErrorContains(t, err, "API_TOKEN")
	}
}

// LogAttrs is the one place configuration is printed; a token is a
// credential, so only the count may appear.
func TestLogAttrsHidesAPITokens(t *testing.T) {
	cfg := Config{DatabaseURL: "postgres://user:pw@host/db", APITokens: []string{"s3cret-token"}}

	var dump strings.Builder
	for _, a := range cfg.LogAttrs() {
		dump.WriteString(a.Key)
		dump.WriteByte('=')
		dump.WriteString(a.Value.String())
		dump.WriteByte('\n')
	}
	blob := dump.String()

	assert.NotContains(t, blob, "s3cret-token")
	assert.Contains(t, blob, "api_token_count=1")
}

func TestLoadConfigEncryptionKey(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	key := bytes.Repeat([]byte{7}, 32)

	t.Setenv("CONFIG_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, key, cfg.ConfigEncryptionKey)

	// An unpadded key is accepted too; trimming the trailing '=' from
	// `openssl rand -base64 32` is an easy mistake.
	t.Setenv("CONFIG_ENCRYPTION_KEY", base64.RawStdEncoding.EncodeToString(key))
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, key, cfg.ConfigEncryptionKey)
}

func TestLoadRejectsInvalidConfigEncryptionKey(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	t.Setenv("CONFIG_ENCRYPTION_KEY", "not base64 !!!")
	_, err := Load()
	assert.ErrorContains(t, err, "CONFIG_ENCRYPTION_KEY")
	assert.ErrorContains(t, err, "base64")

	// The wrong length must fail at startup, not on the first channel write.
	t.Setenv("CONFIG_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 20)))
	_, err = Load()
	assert.ErrorContains(t, err, "CONFIG_ENCRYPTION_KEY")
	assert.ErrorContains(t, err, "16, 24 or 32")
}

func TestLogAttrsHidesConfigEncryptionKey(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	cfg := Config{
		DatabaseURL:         "postgres://user:pw@host/db",
		HTTPAddr:            ":8080",
		ConfigEncryptionKey: key,
	}

	var dump strings.Builder
	for _, a := range cfg.LogAttrs() {
		dump.WriteString(a.Key)
		dump.WriteByte('=')
		dump.WriteString(a.Value.String())
		dump.WriteByte('\n')
	}
	blob := dump.String()

	assert.NotContains(t, blob, string(key))
	assert.NotContains(t, blob, base64.StdEncoding.EncodeToString(key))
	assert.Contains(t, blob, "config_encryption_enabled=true")
}

func TestLoadDatabasePoolSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("DATABASE_MAX_CONNS", "8")
	t.Setenv("DATABASE_MIN_CONNS", "2")
	t.Setenv("DATABASE_MAX_CONN_LIFETIME", "1h")
	t.Setenv("DATABASE_MAX_CONN_IDLE_TIME", "10m")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, int32(8), cfg.DatabaseMaxConns)
	assert.Equal(t, int32(2), cfg.DatabaseMinConns)
	assert.Equal(t, time.Hour, cfg.DatabaseMaxConnLifetime)
	assert.Equal(t, 10*time.Minute, cfg.DatabaseMaxConnIdleTime)
}

func TestLoadDatabasePoolZeroMeansDriverDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("DATABASE_MAX_CONNS", "0")
	t.Setenv("DATABASE_MIN_CONNS", "0")
	t.Setenv("DATABASE_MAX_CONN_LIFETIME", "0s")
	t.Setenv("DATABASE_MAX_CONN_IDLE_TIME", "0s")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Zero(t, cfg.DatabaseMaxConns)
	assert.Zero(t, cfg.DatabaseMinConns)
	assert.Zero(t, cfg.DatabaseMaxConnLifetime)
	assert.Zero(t, cfg.DatabaseMaxConnIdleTime)
}

func TestLoadRejectsNegativeDatabasePoolSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	t.Setenv("DATABASE_MAX_CONNS", "-1")
	_, err := Load()
	assert.ErrorContains(t, err, "DATABASE_MAX_CONNS")
	assert.ErrorContains(t, err, "negative")

	t.Setenv("DATABASE_MAX_CONNS", "4")
	t.Setenv("DATABASE_MIN_CONNS", "-2")
	_, err = Load()
	assert.ErrorContains(t, err, "DATABASE_MIN_CONNS")
	assert.ErrorContains(t, err, "negative")

	t.Setenv("DATABASE_MIN_CONNS", "1")
	t.Setenv("DATABASE_MAX_CONN_LIFETIME", "-1s")
	_, err = Load()
	assert.ErrorContains(t, err, "DATABASE_MAX_CONN_LIFETIME")
	assert.ErrorContains(t, err, "negative")

	t.Setenv("DATABASE_MAX_CONN_LIFETIME", "1h")
	t.Setenv("DATABASE_MAX_CONN_IDLE_TIME", "-5m")
	_, err = Load()
	assert.ErrorContains(t, err, "DATABASE_MAX_CONN_IDLE_TIME")
	assert.ErrorContains(t, err, "negative")
}

func TestLoadRejectsMaxConnsBelowMinConns(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("DATABASE_MAX_CONNS", "2")
	t.Setenv("DATABASE_MIN_CONNS", "5")

	_, err := Load()
	assert.ErrorContains(t, err, "DATABASE_MAX_CONNS 2")
	assert.ErrorContains(t, err, "DATABASE_MIN_CONNS 5")
}

func TestLoadRejectsInvalidDatabasePoolValues(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	t.Setenv("DATABASE_MAX_CONNS", "plenty")
	_, err := Load()
	assert.ErrorContains(t, err, "DATABASE_MAX_CONNS")

	t.Setenv("DATABASE_MAX_CONNS", "4")
	t.Setenv("DATABASE_MAX_CONN_LIFETIME", "forever")
	_, err = Load()
	assert.ErrorContains(t, err, "DATABASE_MAX_CONN_LIFETIME")
}

func TestValidateDatabaseURL(t *testing.T) {
	const secret = "s3cret-password"
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		want    []string
	}{
		{name: "valid postgres", raw: "postgres://user:" + secret + "@localhost:5432/sorobeacon?sslmode=disable"},
		{name: "valid postgresql", raw: "postgresql://user:" + secret + "@db.example:5432/app"},
		{
			name:    "missing",
			raw:     "",
			wantErr: true,
			want:    []string{"DATABASE_URL", "postgres://"},
		},
		{
			name:    "unparseable",
			raw:     "http://[",
			wantErr: true,
			want:    []string{"DATABASE_URL", "postgres", "postgresql"},
		},
		{
			name:    "unsupported scheme",
			raw:     "mysql://user:" + secret + "@localhost:3306/db",
			wantErr: true,
			want:    []string{"DATABASE_URL", "mysql", "localhost", "postgres", "postgresql"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDatabaseURL(tt.raw)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.NotContains(t, err.Error(), secret)
			for _, s := range tt.want {
				assert.ErrorContains(t, err, s)
			}
		})
	}
}

func TestLoadAcceptsValidDatabaseURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "postgres", raw: "postgres://user:s3cret-password@localhost:5432/sorobeacon?sslmode=disable"},
		{name: "postgresql", raw: "postgresql://user:s3cret-password@db.example:5432/app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", tt.raw)

			cfg, err := Load()

			require.NoError(t, err)
			assert.Equal(t, tt.raw, cfg.DatabaseURL)
		})
	}
}

// DATABASE_URL=sqlite://... must load: it is the backend that lets a
// single-node deployment run without Postgres at all.
func TestLoadAcceptsSQLiteDatabaseURL(t *testing.T) {
	const url = "sqlite:///var/lib/sorobeacon/sorobeacon.db"
	t.Setenv("DATABASE_URL", url)
	t.Setenv("DATABASE_MAX_CONNS", "")
	t.Setenv("DATABASE_MIN_CONNS", "")
	t.Setenv("DATABASE_MAX_CONN_LIFETIME", "")
	t.Setenv("DATABASE_MAX_CONN_IDLE_TIME", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, url, cfg.DatabaseURL)
}

func TestValidateDatabaseURLSQLite(t *testing.T) {
	for _, raw := range []string{
		"sqlite:///var/lib/sorobeacon/sorobeacon.db",
		"sqlite://relative.db",
		"sqlite:./data/sorobeacon.db",
	} {
		require.NoError(t, validateDatabaseURL(raw), raw)
	}
	err := validateDatabaseURL("sqlite://")
	require.Error(t, err, "a sqlite URL with no file path cannot work")
	assert.ErrorContains(t, err, "file path")
}

// The pool knobs tune the Postgres connection pool. Carrying them into a
// SQLite deployment would silently do nothing, so Load fails loudly instead.
func TestLoadRejectsPostgresPoolSettingsWithSQLite(t *testing.T) {
	t.Setenv("DATABASE_URL", "sqlite:///tmp/sorobeacon.db")
	t.Setenv("DATABASE_MAX_CONNS", "4")

	_, err := Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "DATABASE_MAX_CONNS")
	assert.ErrorContains(t, err, "sqlite")
}

// Read-replica routing is off unless REPLICA_DATABASE_URL says otherwise, and
// the URL it carries is the value the store is handed.
func TestLoadReplicaDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("REPLICA_DATABASE_URL", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.ReplicaDatabaseURL, "no replica URL means every read stays on the primary")

	t.Setenv("REPLICA_DATABASE_URL", "  postgres://replica:5432/sorobeacon  ")

	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://replica:5432/sorobeacon", cfg.ReplicaDatabaseURL, "surrounding space is trimmed")
}

// A replica URL has to be a Postgres URL, and it has to be a different one
// from the primary: pointing both at the same string looks like routing is on
// in every log line and metric while every read still lands on the primary.
// Both are startup errors rather than warnings.
func TestLoadRejectsInvalidReplicaDatabaseURL(t *testing.T) {
	const secret = "s3cret-password"

	tests := []struct {
		name    string
		replica string
		want    []string
	}{
		{
			name:    "unsupported scheme",
			replica: "mysql://user:" + secret + "@localhost:3306/db",
			want:    []string{"REPLICA_DATABASE_URL", "mysql", "Postgres"},
		},
		{
			name:    "not a URL",
			replica: "replica-host:5432",
			want:    []string{"REPLICA_DATABASE_URL", "parseable"},
		},
		{
			name:    "missing host",
			replica: "postgres:///sorobeacon",
			want:    []string{"REPLICA_DATABASE_URL", "parseable"},
		},
		{
			name:    "identical to the primary",
			replica: "postgres://x",
			want:    []string{"REPLICA_DATABASE_URL", "identical", "DATABASE_URL"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv("REPLICA_DATABASE_URL", tt.replica)

			_, err := Load()

			require.Error(t, err)
			for _, want := range tt.want {
				assert.ErrorContains(t, err, want)
			}
			assert.NotContains(t, err.Error(), secret, "a credential must not reach the error text")
		})
	}
}

// SQLite serves reads and writes from one file handle, so there is no replica
// to route to. Carrying the variable into a SQLite deployment would silently
// do nothing, so Load fails loudly instead — matching the pool knobs above.
func TestLoadRejectsReplicaDatabaseURLWithSQLite(t *testing.T) {
	t.Setenv("DATABASE_URL", "sqlite:///tmp/sorobeacon.db")
	t.Setenv("REPLICA_DATABASE_URL", "postgres://replica:5432/sorobeacon")

	_, err := Load()

	require.Error(t, err)
	assert.ErrorContains(t, err, "REPLICA_DATABASE_URL")
	assert.ErrorContains(t, err, "sqlite")
}

// The replica URL carries its own credentials, so the startup log line gets
// the same treatment as DATABASE_URL: scheme, host and database name only.
func TestLogAttrsRedactsReplicaDatabaseURL(t *testing.T) {
	const secret = "s3cret-password"
	cfg := Config{
		DatabaseURL:        "postgres://user:" + secret + "@primary:5432/sorobeacon",
		ReplicaDatabaseURL: "postgres://user:" + secret + "@replica:5432/sorobeacon",
	}

	var dump strings.Builder
	for _, a := range cfg.LogAttrs() {
		dump.WriteString(a.Key)
		dump.WriteByte('=')
		dump.WriteString(a.Value.String())
		dump.WriteByte('\n')
	}
	blob := dump.String()

	assert.NotContains(t, blob, secret, "a replica credential must not reach the log line")
	assert.Contains(t, blob, "replica_database_url=postgres://replica:5432/sorobeacon")
}

// A SQLite URL holds no credentials, so the log line keeps the file path
// rather than replacing the whole URL with [redacted].
func TestRedactDatabaseURLSQLiteKeepsPath(t *testing.T) {
	assert.Equal(t, "sqlite:///var/lib/sorobeacon/sorobeacon.db",
		redactDatabaseURL("sqlite:///var/lib/sorobeacon/sorobeacon.db"))
	assert.Equal(t, "sqlite://relative.db", redactDatabaseURL("sqlite://relative.db?cache=shared"))
}

func TestLoadRejectsInvalidDatabaseURL(t *testing.T) {
	const secret = "s3cret-password"
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "unparseable", raw: "http://[", want: []string{"DATABASE_URL", "parseable"}},
		{name: "unsupported scheme", raw: "mysql://user:" + secret + "@localhost:3306/db", want: []string{"DATABASE_URL", "mysql", "localhost"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", tt.raw)

			_, err := Load()

			require.Error(t, err)
			assert.NotContains(t, err.Error(), secret)
			for _, s := range tt.want {
				assert.ErrorContains(t, err, s)
			}
		})
	}
}

func TestLoadSecretsProvider(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	// Disabled by default: references stay literals.
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.SecretsProvider)

	t.Setenv("SECRETS_PROVIDER", "env")
	t.Setenv("SECRETS_CACHE_TTL", "90s")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "env", cfg.SecretsProvider)
	assert.Equal(t, 90*time.Second, cfg.SecretsCacheTTL)

	// A disabled cache is allowed.
	t.Setenv("SECRETS_CACHE_TTL", "0s")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Zero(t, cfg.SecretsCacheTTL)
}

func TestLoadRejectsBadSecretsConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")

	t.Setenv("SECRETS_PROVIDER", "aws")
	_, err := Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "SECRETS_PROVIDER")

	t.Setenv("SECRETS_PROVIDER", "")
	t.Setenv("SECRETS_CACHE_TTL", "-1s")
	_, err = Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "SECRETS_CACHE_TTL")

	t.Setenv("SECRETS_CACHE_TTL", "")
	t.Setenv("SECRETS_PROVIDER", "vault")
	_, err = Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "VAULT_ADDR")

	// The token is a credential and must never reach LogAttrs.
	t.Setenv("VAULT_ADDR", "https://vault.example:8200")
	t.Setenv("VAULT_TOKEN", "hvs.super-secret")
	cfg, err := Load()
	require.NoError(t, err)
	for _, a := range cfg.LogAttrs() {
		assert.NotEqual(t, "hvs.super-secret", a.Value.String())
	}
}
