// Package config loads SoroBeacon configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults used when the corresponding environment variable is unset.
const (
	DefaultRPCURL       = "https://soroban-testnet.stellar.org"
	DefaultPollInterval = 5 * time.Second
	DefaultHTTPAddr     = ":8080"
	// DefaultHTTPMaxBodyBytes is 1 MiB. Rule params are nested JSON and
	// channel configs are small; 1 MiB is well above any legitimate write
	// payload while bounding unauthenticated POSTs on a small instance.
	DefaultHTTPMaxBodyBytes int64 = 1 << 20
	// DefaultMonitorSilentAfter is how long since last_matched_at before
	// the monitors list treats a monitor as silent.
	DefaultMonitorSilentAfter = 24 * time.Hour
)

// Config holds all runtime configuration. Every field maps to one
// environment variable; see .env.example for the full list.
type Config struct {
	// Network is the Stellar network to monitor — its name, passphrase
	// and RPC endpoint, resolved from NETWORK / RPC_URL /
	// NETWORK_PASSPHRASE by ParseNetwork.
	Network Network
	// RPCURL is the Stellar RPC endpoint (JSON-RPC 2.0 over HTTP). This is
	// Network.RPCURL; kept as a direct field since most call sites only
	// need the URL.
	RPCURL string
	// DatabaseURL is a Postgres connection string (pgx format).
	DatabaseURL string
	// DatabaseMaxConns is the pgx pool MaxConns. Zero means use the
	// driver default (DATABASE_MAX_CONNS).
	DatabaseMaxConns int32
	// DatabaseMinConns is the pgx pool MinConns. Zero means use the
	// driver default (DATABASE_MIN_CONNS).
	DatabaseMinConns int32
	// DatabaseMaxConnLifetime is the pgx pool MaxConnLifetime. Zero
	// means use the driver default (DATABASE_MAX_CONN_LIFETIME).
	DatabaseMaxConnLifetime time.Duration
	// DatabaseMaxConnIdleTime is the pgx pool MaxConnIdleTime. Zero
	// means use the driver default (DATABASE_MAX_CONN_IDLE_TIME).
	DatabaseMaxConnIdleTime time.Duration
	// PollInterval is how often the poller asks the RPC for new events.
	PollInterval time.Duration
	// SourceMode selects where events come from: "rpc" (standalone,
	// default) or "sorotrail" (upstream, reads a SoroTrail indexer).
	SourceMode string
	// SoroTrailURL is the base URL of a SoroTrail indexer; required when
	// SourceMode is "sorotrail", ignored otherwise.
	SoroTrailURL string
	// CORSAllowedOrigins is the allow-list of browser Origins permitted to
	// call the API cross-origin (CORS_ALLOWED_ORIGINS, comma-separated).
	// Empty disables CORS; the dashboard is same-origin and never needs it.
	CORSAllowedOrigins []string
	// HTTPAddr is the listen address for the API and dashboard.
	HTTPAddr string
	// HTTPMaxBodyBytes is the maximum request body size accepted by API
	// write endpoints (HTTP_MAX_BODY_BYTES). GET/HEAD/OPTIONS are not
	// limited. Zero is not a valid configured value; Load always sets a
	// positive default.
	HTTPMaxBodyBytes int64
	// LogLevel is the minimum slog level (debug, info, warn, error).
	LogLevel slog.Level
	// ReadyzLagThreshold is the ledger lag at which /readyz fails.
	// Zero (the default) disables the check so existing probes stay green.
	ReadyzLagThreshold uint32
	// RateLimitRPS is the per-client API token-bucket refill rate.
	// Zero (default) disables the limiter.
	RateLimitRPS float64
	// RateLimitBurst is the per-client bucket size. When the limiter is
	// enabled and this is zero, Load defaults it to max(1, ceil(RPS)).
	RateLimitBurst int
	// RateLimitTrustForwarded keys clients by the first X-Forwarded-For
	// address. Default false: a spoofed header would otherwise defeat the
	// limit. Only enable this behind a proxy that overwrites the header.
	RateLimitTrustForwarded bool
	// AlertRetention is how long alerts (and cascaded delivery_attempts)
	// are kept. Zero (the default, when ALERT_RETENTION is unset) keeps
	// everything forever so upgrades never start deleting history.
	AlertRetention time.Duration
	// MonitorSilentAfter is how long since last_matched_at before the
	// dashboard marks a monitor silent. Default 24h.
	MonitorSilentAfter time.Duration
}

// Load reads configuration from the environment. DATABASE_URL is the only
// required variable; everything else has a sensible default.
func Load() (Config, error) {
	net, err := ParseNetwork(os.Getenv)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Network:            net,
		RPCURL:             net.RPCURL,
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		PollInterval:       DefaultPollInterval,
		HTTPAddr:           getenv("HTTP_ADDR", DefaultHTTPAddr),
		HTTPMaxBodyBytes:   DefaultHTTPMaxBodyBytes,
		LogLevel:           slog.LevelInfo,
		MonitorSilentAfter: DefaultMonitorSilentAfter,
	}

	if err := validateDatabaseURL(cfg.DatabaseURL); err != nil {
		return cfg, err
	}

	if err := validateHTTPAddr(cfg.HTTPAddr); err != nil {
		return cfg, err
	}

	// An absolute http(s) RPC URL is required whenever one is in play —
	// always in rpc mode, and in sorotrail mode whenever RPC_URL is set
	// alongside the indexer URL.
	rpcURL, err := url.Parse(cfg.RPCURL)
	if cfg.RPCURL != "" && (err != nil || !rpcURL.IsAbs() || rpcURL.Host == "" ||
		(rpcURL.Scheme != "http" && rpcURL.Scheme != "https")) {
		return cfg, fmt.Errorf(
			"invalid RPC_URL %q: must be an absolute http or https URL",
			cfg.RPCURL,
		)
	}

	cfg.SourceMode = getenv("SOURCE_MODE", "rpc")
	if cfg.SourceMode != "rpc" && cfg.SourceMode != "sorotrail" {
		return cfg, fmt.Errorf("invalid SOURCE_MODE %q (want rpc|sorotrail)", cfg.SourceMode)
	}
	cfg.SoroTrailURL = os.Getenv("SOROTRAIL_URL")
	if cfg.SourceMode == "sorotrail" && cfg.SoroTrailURL == "" {
		return cfg, fmt.Errorf("SOROTRAIL_URL is required when SOURCE_MODE=sorotrail")
	}

	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid POLL_INTERVAL %q: %w", v, err)
		}
		if d < time.Second {
			return cfg, fmt.Errorf("POLL_INTERVAL %q is below the 1s minimum", v)
		}
		cfg.PollInterval = d
	}

	if v := os.Getenv("CORS_ALLOWED_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				cfg.CORSAllowedOrigins = append(cfg.CORSAllowedOrigins, o)
			}
		}
	}

	if v := os.Getenv("HTTP_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid HTTP_MAX_BODY_BYTES %q: must be a positive integer (bytes)", v)
		}
		cfg.HTTPMaxBodyBytes = n
	}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		lvl, err := parseLevel(v)
		if err != nil {
			return cfg, err
		}
		cfg.LogLevel = lvl
	}

	if v := os.Getenv("READYZ_LAG_THRESHOLD"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return cfg, fmt.Errorf("invalid READYZ_LAG_THRESHOLD %q: %w", v, err)
		}
		cfg.ReadyzLagThreshold = uint32(n)
	}
	if v := os.Getenv("RATE_LIMIT_RPS"); v != "" {
		rps, err := strconv.ParseFloat(v, 64)
		if err != nil || rps < 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
			return cfg, fmt.Errorf("invalid RATE_LIMIT_RPS %q (want a non-negative number)", v)
		}
		cfg.RateLimitRPS = rps
	}
	if v := os.Getenv("RATE_LIMIT_BURST"); v != "" {
		burst, err := strconv.Atoi(v)
		if err != nil || burst < 0 {
			return cfg, fmt.Errorf("invalid RATE_LIMIT_BURST %q (want a non-negative integer)", v)
		}
		cfg.RateLimitBurst = burst
	}
	if cfg.RateLimitRPS > 0 && cfg.RateLimitBurst == 0 {
		cfg.RateLimitBurst = int(math.Ceil(cfg.RateLimitRPS))
		if cfg.RateLimitBurst < 1 {
			cfg.RateLimitBurst = 1
		}
	}
	if v := os.Getenv("MONITOR_SILENT_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid MONITOR_SILENT_AFTER %q: %w", v, err)
		}
		if d <= 0 {
			return cfg, fmt.Errorf("MONITOR_SILENT_AFTER %q must be greater than 0", v)
		}
		cfg.MonitorSilentAfter = d
	}

	if v := os.Getenv("RATE_LIMIT_TRUST_FORWARDED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			cfg.RateLimitTrustForwarded = true
		case "0", "false", "no", "off":
			cfg.RateLimitTrustForwarded = false
		default:
			return cfg, fmt.Errorf("invalid RATE_LIMIT_TRUST_FORWARDED %q (want true|false)", v)
		}
	}
	maxConns, err := parseInt32Env("DATABASE_MAX_CONNS")
	if err != nil {
		return cfg, err
	}
	minConns, err := parseInt32Env("DATABASE_MIN_CONNS")
	if err != nil {
		return cfg, err
	}
	maxLifetime, err := parseDurationEnv("DATABASE_MAX_CONN_LIFETIME")
	if err != nil {
		return cfg, err
	}
	maxIdle, err := parseDurationEnv("DATABASE_MAX_CONN_IDLE_TIME")
	if err != nil {
		return cfg, err
	}
	if maxConns > 0 && minConns > 0 && maxConns < minConns {
		return cfg, fmt.Errorf("DATABASE_MAX_CONNS %d is below DATABASE_MIN_CONNS %d", maxConns, minConns)
	}
	cfg.DatabaseMaxConns = maxConns
	cfg.DatabaseMinConns = minConns
	cfg.DatabaseMaxConnLifetime = maxLifetime
	cfg.DatabaseMaxConnIdleTime = maxIdle
	if v := os.Getenv("ALERT_RETENTION"); v != "" {
		d, err := ParseRetention(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid ALERT_RETENTION %q: %w", v, err)
		}
		cfg.AlertRetention = d
	}

	return cfg, nil
}

// validateHTTPAddr checks HTTP_ADDR is a host:port pair with a numeric
// port in 0–65535. An empty host is valid (:8080 listens on all
// interfaces). Called from Load so a bad listen address fails before
// migrations or the poller start.
func validateHTTPAddr(addr string) error {
	const form = "must be host:port with a numeric port 0-65535 (e.g. :8080 or 127.0.0.1:8080)"
	if addr == "" {
		return fmt.Errorf("invalid HTTP_ADDR %q: %s", addr, form)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid HTTP_ADDR %q: %s", addr, form)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("invalid HTTP_ADDR %q: %s", addr, form)
	}
	return nil
}

const redacted = "[redacted]"

// LogAttrs returns the effective configuration as slog attributes.
// Fields are opted in: a new Config field is not logged until it is
// listed here, so a secret cannot leak by accident.
func (c Config) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("database_url", redactDatabaseURL(c.DatabaseURL)),
		slog.String("http_addr", c.HTTPAddr),
		slog.String("source_mode", c.SourceMode),
		slog.String("poll_interval", c.PollInterval.String()),
		slog.String("log_level", strings.ToLower(c.LogLevel.String())),
		slog.String("network", c.Network.Name),
		slog.String("rpc_url", c.RPCURL),
		slog.String("sorotrail_url", c.SoroTrailURL),
		slog.String("cors_allowed_origins", strings.Join(c.CORSAllowedOrigins, ",")),
	}
}

// redactDatabaseURL keeps scheme, host (with port) and database name and
// drops userinfo, query and fragment so a password never appears in logs.
func redactDatabaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return redacted
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// ParseRetention accepts Go durations (24h, 90m) plus a day suffix
// (90d) used in ALERT_RETENTION. Empty is zero (keep forever). A
// non-positive duration is rejected so operators cannot accidentally
// prune everything.
func ParseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err == nil {
			if n <= 0 {
				return 0, fmt.Errorf("must be a positive duration")
			}
			return time.Duration(n * 24 * float64(time.Hour)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be a positive duration")
	}
	return d, nil
}

const databaseURLExample = "postgres://user:pass@localhost:5432/dbname?sslmode=disable"

// validateDatabaseURL checks DATABASE_URL before any connection attempt so a
// missing or malformed value fails at config.Load instead of as a driver
// error that looks like the database is down. Errors name the variable and
// never echo the raw value (it holds a password); scheme and host are safe
// to show once the URL has parsed.
func validateDatabaseURL(raw string) error {
	const supported = "supported schemes: postgres, postgresql"
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("DATABASE_URL is required (e.g. %s)", databaseURLExample)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("DATABASE_URL is not a parseable URL (%s)", supported)
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
		return nil
	default:
		return fmt.Errorf("DATABASE_URL scheme %q (host %s) is not supported (%s)", u.Scheme, u.Host, supported)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseInt32Env reads an optional int32. Unset or empty is 0 (driver
// default). Negative values are rejected so a typo cannot shrink the
// pool below pgx's floor.
func parseInt32Env(key string) (int32, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s %d is negative", key, n)
	}
	return int32(n), nil
}

// parseDurationEnv reads an optional duration. Unset or empty is 0
// (driver default). Negative durations are rejected.
func parseDurationEnv(key string) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s %q is negative", key, v)
	}
	return d, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("invalid LOG_LEVEL %q (want debug|info|warn|error)", s)
}
