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
	assert.Zero(t, cfg.DatabaseMaxConns)
	assert.Zero(t, cfg.DatabaseMinConns)
	assert.Zero(t, cfg.DatabaseMaxConnLifetime)
	assert.Zero(t, cfg.DatabaseMaxConnIdleTime)
	assert.Equal(t, time.Duration(0), cfg.AlertRetention)
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
