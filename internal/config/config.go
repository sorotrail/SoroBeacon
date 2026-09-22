// Package config loads SoroBeacon configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"math"
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
}

// Load reads configuration from the environment. DATABASE_URL is the only
// required variable; everything else has a sensible default.
func Load() (Config, error) {
	net, err := ParseNetwork(os.Getenv)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Network:      net,
		RPCURL:       net.RPCURL,
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		PollInterval: DefaultPollInterval,
		HTTPAddr:     getenv("HTTP_ADDR", DefaultHTTPAddr),
		LogLevel:     slog.LevelInfo,
	}

	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
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

	return cfg, nil
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
