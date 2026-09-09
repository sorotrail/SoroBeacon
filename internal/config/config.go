// Package config loads SoroBeacon configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
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
	// PollInterval is how often the poller asks the RPC for new events.
	PollInterval time.Duration
	// HTTPAddr is the listen address for the API and dashboard.
	HTTPAddr string
	// LogLevel is the minimum slog level (debug, info, warn, error).
	LogLevel slog.Level
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

	rpcURL, err := url.Parse(cfg.RPCURL)
	if err != nil || !rpcURL.IsAbs() || rpcURL.Host == "" ||
		(rpcURL.Scheme != "http" && rpcURL.Scheme != "https") {
		return cfg, fmt.Errorf(
			"invalid RPC_URL %q: must be an absolute http or https URL",
			cfg.RPCURL,
		)
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

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		lvl, err := parseLevel(v)
		if err != nil {
			return cfg, err
		}
		cfg.LogLevel = lvl
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
