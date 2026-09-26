// Package config loads SoroBeacon configuration from environment variables.
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/secrets"
)

// Defaults used when the corresponding environment variable is unset.
const (
	DefaultRPCURL       = "https://soroban-testnet.stellar.org"
	DefaultPollInterval = 5 * time.Second
	DefaultHTTPAddr     = ":8080"
	// DefaultOTLPSampleRate keeps every trace when tracing is enabled.
	// Sampling is an operator lever for busy deployments, not a way to hide
	// spans by default: the whole feature is off unless OTLP_ENDPOINT is set.
	DefaultOTLPSampleRate = 1.0
	// DefaultHTTPMaxBodyBytes is 1 MiB. Rule params are nested JSON and
	// channel configs are small; 1 MiB is well above any legitimate write
	// payload while bounding unauthenticated POSTs on a small instance.
	DefaultHTTPMaxBodyBytes int64 = 1 << 20
	// DefaultMonitorSilentAfter is how long since last_matched_at before
	// the monitors list treats a monitor as silent.
	DefaultMonitorSilentAfter = 24 * time.Hour
	// DefaultReorgTrackingWindow is how many recent ledger hashes the poller
	// keeps for reorg detection. 128 ledgers is roughly ten minutes on
	// Stellar and a few getLedgers pages per cycle — cheap, and deep enough
	// to cover the practical reorg depth.
	DefaultReorgTrackingWindow uint32 = 128
)

// Config holds all runtime configuration. Every field maps to one
// environment variable; see .env.example for the full list.
type Config struct {
	// Network is the Stellar network to monitor — its name, passphrase
	// and RPC endpoint, resolved from NETWORK / RPC_URL /
	// NETWORK_PASSPHRASE by ParseNetwork.
	Network Network
	// RPCURL is the first Stellar RPC endpoint (JSON-RPC 2.0 over HTTP).
	// This is Network.RPCURL; kept as a direct field since most call sites
	// only need the URL.
	RPCURL string
	// RPCURLs is the ordered list of Stellar RPC endpoints to poll, with
	// failover in that order (RPC_URLS). It is always non-empty: RPC_URLS
	// takes priority when set, and RPC_URL alone is the single-entry case,
	// so a deployment that never sets RPC_URLS behaves exactly as before.
	RPCURLs []string
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
	// ReplicaDatabaseURL is a second Postgres connection string serving the
	// read-only queries the store routes (REPLICA_DATABASE_URL). Empty means
	// no replica: every read stays on DatabaseURL, which is how every
	// deployment behaved before replica routing existed.
	ReplicaDatabaseURL string
	// ConfigEncryptionKey is the decoded AES-GCM key used to encrypt
	// channels.config at rest (CONFIG_ENCRYPTION_KEY, base64). Nil means
	// encryption is disabled and configs stay plaintext, preserving the
	// behaviour of deployments that have not set the variable.
	ConfigEncryptionKey []byte
	// APITokens is the parsed API_TOKEN list (API_TOKEN, comma-separated).
	// Each entry is a static bearer token accepted on /api/v1 and can be
	// used to sign in to the dashboard. Empty means no token is configured:
	// the API and the dashboard stay open, exactly as they were before
	// authentication existed, and the process logs one startup warning.
	// Hold the tokens here, not the raw string: the values are secrets and
	// must never be logged or echoed.
	APITokens []string
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
	// OTLP is the OpenTelemetry tracing configuration. Endpoint empty (the
	// default, OTLP_ENDPOINT unset) disables tracing entirely: no exporter,
	// no exporter goroutines, no measurable overhead — spans collapse to
	// no-ops. When set it is the OTLP/HTTP base URL spans are shipped to.
	OTLP OTLPConfig
	// GRPCAddr is the listen address for the optional gRPC server
	// (GRPC_ADDR). Empty (the default) disables gRPC entirely so existing
	// deployments do not open a new port without opting in.
	GRPCAddr string
	// ReorgTrackingWindow is how many recent ledgers' hashes the poller keeps
	// and re-checks each cycle for reorg detection
	// (REORG_TRACKING_WINDOW, default 128). Zero disables detection, which is
	// the behaviour before the feature existed.
	ReorgTrackingWindow uint32
	// ReorgConfirmationDepth is how many ledgers behind the tip an event must
	// be before it may alert (REORG_CONFIRMATION_DEPTH, default 0). Zero
	// alerts immediately, the historical default.
	ReorgConfirmationDepth uint32
	// SecretsProvider selects the external secret provider used to resolve
	// ${secret:...} references in channel configs (SECRETS_PROVIDER).
	// Empty (the default) disables external secrets: references are then
	// treated as literals, so an upgrade changes nothing.
	SecretsProvider string
	// SecretsCacheTTL is how long a resolved secret is reused
	// (SECRETS_CACHE_TTL, default secrets.DefaultTTL). Zero disables
	// caching so every construction re-fetches.
	SecretsCacheTTL time.Duration
	// VaultAddr is the HashiCorp Vault address (VAULT_ADDR); required when
	// SecretsProvider is "vault".
	VaultAddr string
	// VaultToken is the Vault token (VAULT_TOKEN). It is a credential and
	// is never logged.
	VaultToken string
	// VaultNamespace is the optional Vault Enterprise namespace
	// (VAULT_NAMESPACE).
	VaultNamespace string
	// ArchiveURL is where retention copies alerts before deleting them
	// (ARCHIVE_URL). Empty (the default) leaves archiving off, so retention
	// behaves exactly as it did before the feature. A local directory path,
	// file://, dir:// or s3://bucket/prefix are accepted; the archive package
	// validates it when the pruner is built.
	ArchiveURL string

	// NotifyRateLimitSlackRPS is the max requests per second for Slack channels
	// (NOTIFY_RATE_LIMIT_SLACK_RPS, default 1.0, citing Slack API tier 2 / webhooks guidelines ~1 msg/sec).
	NotifyRateLimitSlackRPS float64
	// NotifyRateLimitTelegramRPS is the max requests per second for Telegram channels
	// (NOTIFY_RATE_LIMIT_TELEGRAM_RPS, default 30.0, citing Telegram Bot API limit of 30 msg/sec).
	NotifyRateLimitTelegramRPS float64
	// NotifyRateLimitPagerDutyRPS is the max requests per second for PagerDuty channels
	// (NOTIFY_RATE_LIMIT_PAGERDUTY_RPS, default 2.0, citing PagerDuty Events API v2 rate limit ~2 requests/sec).
	NotifyRateLimitPagerDutyRPS float64
	// NotifyRateLimitDefaultRPS is the default max requests per second for any other channel
	// (NOTIFY_RATE_LIMIT_DEFAULT_RPS, default 5.0).
	NotifyRateLimitDefaultRPS float64
}

// OTLPConfig is the tracing slice of the configuration. It is a struct so a
// stage that needs the endpoint does not pull the whole Config in.
type OTLPConfig struct {
	// Endpoint is the OTLP/HTTP base URL (OTLP_ENDPOINT, e.g.
	// http://localhost:4318). Empty disables tracing.
	Endpoint string
	// ServiceName is the service.name resource attribute
	// (OTLP_SERVICE_NAME). Empty falls back to "sorobeacon".
	ServiceName string
	// SampleRate is the fraction of traces kept, in [0,1]
	// (OTLP_SAMPLE_RATE). Defaults to 1 (keep everything).
	SampleRate float64
}

// Enabled reports whether tracing is switched on. The single place the
// "off unless OTLP_ENDPOINT is set" rule lives, so the wiring in main and
// the tests cannot drift apart.
func (o OTLPConfig) Enabled() bool { return o.Endpoint != "" }

// Load reads configuration from the environment. DATABASE_URL is the only
// required variable; everything else has a sensible default.
func Load() (Config, error) {
	net, err := ParseNetwork(os.Getenv)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Network:                     net,
		RPCURL:                      net.RPCURL,
		RPCURLs:                     net.RPCURLs,
		DatabaseURL:                 os.Getenv("DATABASE_URL"),
		PollInterval:                DefaultPollInterval,
		HTTPAddr:                    getenv("HTTP_ADDR", DefaultHTTPAddr),
		HTTPMaxBodyBytes:            DefaultHTTPMaxBodyBytes,
		LogLevel:                    slog.LevelInfo,
		MonitorSilentAfter:          DefaultMonitorSilentAfter,
		NotifyRateLimitSlackRPS:     1.0,  // Slack webhooks / tier 2 rate limit ~1 rps
		NotifyRateLimitTelegramRPS:  30.0, // Telegram Bot API limit ~30 rps
		NotifyRateLimitPagerDutyRPS: 2.0,  // PagerDuty Events API v2 rate limit ~2 rps
		NotifyRateLimitDefaultRPS:   5.0,  // General default rps
		// Detection is on by default; confirmation depth off, so a monitor
		// alerts exactly as soon as it did before this feature.
		ReorgTrackingWindow:    DefaultReorgTrackingWindow,
		ReorgConfirmationDepth: 0,
	}

	if err := validateDatabaseURL(cfg.DatabaseURL); err != nil {
		return cfg, err
	}
	// The pool knobs below are Postgres-only. Knowing the backend here lets
	// Load fail loudly when a SQLite deployment carries them instead of
	// silently ignoring a setting the operator expects to matter.
	sqliteBackend := isSQLiteURL(cfg.DatabaseURL)

	if err := validateHTTPAddr(cfg.HTTPAddr); err != nil {
		return cfg, err
	}

	// An absolute http(s) RPC URL is required whenever one is in play —
	// always in rpc mode, and in sorotrail mode whenever RPC_URL is set
	// alongside the indexer URL. cfg.RPCURL is RPC_URLS[0] when the list is
	// set, so this covers both spellings; ParseRPCURLs validates the rest of
	// the list (and each entry's own message names it).
	if cfg.RPCURL != "" && !validRPCURL(cfg.RPCURL) {
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

	tokens, err := parseAPITokens(os.Getenv("API_TOKEN"))
	if err != nil {
		return cfg, err
	}
	cfg.APITokens = tokens

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
	if sqliteBackend && (maxConns > 0 || minConns > 0 || maxLifetime > 0 || maxIdle > 0) {
		return cfg, fmt.Errorf("DATABASE_MAX_CONNS, DATABASE_MIN_CONNS, DATABASE_MAX_CONN_LIFETIME and DATABASE_MAX_CONN_IDLE_TIME tune the Postgres pool and have no effect on a sqlite DATABASE_URL; unset them or use Postgres")
	}
	cfg.DatabaseMaxConns = maxConns
	cfg.DatabaseMinConns = minConns
	cfg.DatabaseMaxConnLifetime = maxLifetime
	cfg.DatabaseMaxConnIdleTime = maxIdle

	// Read-replica routing is Postgres-only and off unless asked for. Both
	// failures below are startup errors rather than warnings: an operator who
	// sets REPLICA_DATABASE_URL believes reads are being routed, and a
	// deployment that silently serves everything from the primary while
	// claiming otherwise is worse than one that refuses to boot.
	replicaURL := strings.TrimSpace(os.Getenv("REPLICA_DATABASE_URL"))
	if sqliteBackend && replicaURL != "" {
		return cfg, fmt.Errorf("REPLICA_DATABASE_URL has no effect on a sqlite DATABASE_URL: SQLite serves reads and writes from one file; unset it or use Postgres")
	}
	if replicaURL != "" {
		if err := validateReplicaDatabaseURL(replicaURL, cfg.DatabaseURL); err != nil {
			return cfg, err
		}
	}
	cfg.ReplicaDatabaseURL = replicaURL
	key, err := parseEncryptionKey(os.Getenv("CONFIG_ENCRYPTION_KEY"))
	if err != nil {
		return cfg, err
	}
	cfg.ConfigEncryptionKey = key
	cfg.GRPCAddr = os.Getenv("GRPC_ADDR")
	if cfg.GRPCAddr != "" {
		if err := validateHTTPAddr(cfg.GRPCAddr); err != nil {
			return cfg, fmt.Errorf("invalid GRPC_ADDR: %w", err)
		}
	}

	if v := os.Getenv("ALERT_RETENTION"); v != "" {
		d, err := ParseRetention(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid ALERT_RETENTION %q: %w", v, err)
		}
		cfg.AlertRetention = d
	}
	if v := os.Getenv("REORG_TRACKING_WINDOW"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return cfg, fmt.Errorf("invalid REORG_TRACKING_WINDOW %q: must be a non-negative integer", v)
		}
		cfg.ReorgTrackingWindow = uint32(n)
	}
	if v := os.Getenv("REORG_CONFIRMATION_DEPTH"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return cfg, fmt.Errorf("invalid REORG_CONFIRMATION_DEPTH %q: must be a non-negative integer", v)
		}
		cfg.ReorgConfirmationDepth = uint32(n)
	}
	cfg.ArchiveURL = strings.TrimSpace(os.Getenv("ARCHIVE_URL"))

	// Tracing is off unless OTLP_ENDPOINT is set; see telemetry.Config.
	cfg.OTLP.Endpoint = os.Getenv("OTLP_ENDPOINT")
	if cfg.OTLP.Endpoint != "" {
		u, err := url.Parse(cfg.OTLP.Endpoint)
		if err != nil || !u.IsAbs() || u.Host == "" ||
			(u.Scheme != "http" && u.Scheme != "https") {
			return cfg, fmt.Errorf(
				"invalid OTLP_ENDPOINT %q: must be an absolute http or https URL",
				cfg.OTLP.Endpoint,
			)
		}
	}
	cfg.OTLP.ServiceName = os.Getenv("OTLP_SERVICE_NAME")
	cfg.OTLP.SampleRate = DefaultOTLPSampleRate
	if v := os.Getenv("OTLP_SAMPLE_RATE"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r < 0 || r > 1 || math.IsNaN(r) || math.IsInf(r, 0) {
			return cfg, fmt.Errorf("invalid OTLP_SAMPLE_RATE %q (want a number in [0, 1])", v)
		}
		cfg.OTLP.SampleRate = r
	}
	if v := os.Getenv("NOTIFY_RATE_LIMIT_SLACK_RPS"); v != "" {
		rps, err := strconv.ParseFloat(v, 64)
		if err != nil || rps < 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
			return cfg, fmt.Errorf("invalid NOTIFY_RATE_LIMIT_SLACK_RPS %q (want a non-negative number)", v)
		}
		cfg.NotifyRateLimitSlackRPS = rps
	}
	if v := os.Getenv("NOTIFY_RATE_LIMIT_TELEGRAM_RPS"); v != "" {
		rps, err := strconv.ParseFloat(v, 64)
		if err != nil || rps < 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
			return cfg, fmt.Errorf("invalid NOTIFY_RATE_LIMIT_TELEGRAM_RPS %q (want a non-negative number)", v)
		}
		cfg.NotifyRateLimitTelegramRPS = rps
	}
	if v := os.Getenv("NOTIFY_RATE_LIMIT_PAGERDUTY_RPS"); v != "" {
		rps, err := strconv.ParseFloat(v, 64)
		if err != nil || rps < 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
			return cfg, fmt.Errorf("invalid NOTIFY_RATE_LIMIT_PAGERDUTY_RPS %q (want a non-negative number)", v)
		}
		cfg.NotifyRateLimitPagerDutyRPS = rps
	}
	if v := os.Getenv("NOTIFY_RATE_LIMIT_DEFAULT_RPS"); v != "" {
		rps, err := strconv.ParseFloat(v, 64)
		if err != nil || rps < 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
			return cfg, fmt.Errorf("invalid NOTIFY_RATE_LIMIT_DEFAULT_RPS %q (want a non-negative number)", v)
		}
		cfg.NotifyRateLimitDefaultRPS = rps
	}

	if err := loadSecrets(&cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// loadSecrets reads the external-secret provider configuration. The active
// provider is validated here so a typo or a missing Vault address fails
// startup rather than the first alert that references a secret.
func loadSecrets(cfg *Config) error {
	cfg.SecretsProvider = strings.ToLower(strings.TrimSpace(os.Getenv("SECRETS_PROVIDER")))
	switch cfg.SecretsProvider {
	case "", "env", "vault":
	default:
		return fmt.Errorf("invalid SECRETS_PROVIDER %q (want env|vault, or unset to disable external secrets)", cfg.SecretsProvider)
	}

	cfg.SecretsCacheTTL = secrets.DefaultTTL
	if v := os.Getenv("SECRETS_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid SECRETS_CACHE_TTL %q: %w", v, err)
		}
		if d < 0 {
			return fmt.Errorf("SECRETS_CACHE_TTL %q is negative", v)
		}
		cfg.SecretsCacheTTL = d
	}

	cfg.VaultAddr = strings.TrimRight(strings.TrimSpace(os.Getenv("VAULT_ADDR")), "/")
	cfg.VaultToken = os.Getenv("VAULT_TOKEN")
	cfg.VaultNamespace = strings.TrimSpace(os.Getenv("VAULT_NAMESPACE"))
	if cfg.SecretsProvider == "vault" && cfg.VaultAddr == "" {
		return fmt.Errorf("VAULT_ADDR is required when SECRETS_PROVIDER=vault")
	}
	return nil
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
		// Redacted the same way: the replica URL carries its own credentials,
		// and the redaction keeps only scheme, host and database name.
		slog.String("replica_database_url", redactDatabaseURL(c.ReplicaDatabaseURL)),
		slog.String("http_addr", c.HTTPAddr),
		slog.String("source_mode", c.SourceMode),
		slog.String("poll_interval", c.PollInterval.String()),
		slog.String("log_level", strings.ToLower(c.LogLevel.String())),
		slog.String("network", c.Network.Name),
		slog.String("rpc_url", c.RPCURL),
		// The count, not the list: it is how an operator confirms at a
		// glance that the failover set was read, and the URLs themselves
		// already appear (first one above) in the poller's own lines.
		slog.Int("rpc_endpoint_count", len(c.RPCURLs)),
		slog.String("sorotrail_url", c.SoroTrailURL),
		slog.String("cors_allowed_origins", strings.Join(c.CORSAllowedOrigins, ",")),
		slog.Bool("config_encryption_enabled", len(c.ConfigEncryptionKey) > 0),
		// The provider name, never the token or any resolved value.
		slog.String("secrets_provider", c.SecretsProvider),
		slog.String("secrets_cache_ttl", c.SecretsCacheTTL.String()),
		slog.Bool("vault_token_configured", c.VaultToken != ""),
		slog.Uint64("reorg_tracking_window", uint64(c.ReorgTrackingWindow)),
		slog.Uint64("reorg_confirmation_depth", uint64(c.ReorgConfirmationDepth)),
		// The count, never the tokens themselves: LogAttrs is the one place
		// configuration is printed, and an API token is a credential.
		slog.Int("api_token_count", len(c.APITokens)),
		slog.Bool("otlp_tracing_enabled", c.OTLP.Endpoint != ""),
		slog.String("otlp_service_name", c.OTLP.ServiceName),
		slog.Float64("otlp_sample_rate", c.OTLP.SampleRate),
	}
}

// redactDatabaseURL keeps scheme, host (with port) and database name and
// drops userinfo, query and fragment so a password never appears in logs.
// A SQLite URL carries no credentials, so its file path — the whole database
// — is kept; it is the one field an operator needs in the startup line.
func redactDatabaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return redacted
	}
	if strings.EqualFold(u.Scheme, "sqlite") {
		return u.Scheme + "://" + u.Host + u.Path
	}
	if u.Host == "" {
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

// parseEncryptionKey decodes CONFIG_ENCRYPTION_KEY, a base64-encoded AES-GCM
// key. Empty means encryption is disabled. The key is checked here — before
// the store is built — so a typo fails startup rather than the first channel
// write. Errors name the variable and the allowed lengths but never echo the
// key itself.
func parseEncryptionKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Accept an unpadded key too; operators trimming the trailing '='
		// from `openssl rand -base64 32` is a common mistake.
		key, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(raw, "="))
		if err != nil {
			return nil, fmt.Errorf("invalid CONFIG_ENCRYPTION_KEY: must be standard base64 (e.g. `openssl rand -base64 32`)")
		}
	}
	switch len(key) {
	case 16, 24, 32:
		return key, nil
	default:
		return nil, fmt.Errorf("invalid CONFIG_ENCRYPTION_KEY: must decode to 16, 24 or 32 bytes for AES-GCM (got %d bytes)", len(key))
	}
}

const databaseURLExample = "postgres://user:pass@localhost:5432/dbname?sslmode=disable"

// validateDatabaseURL checks DATABASE_URL before any connection attempt so a
// missing or malformed value fails at config.Load instead of as a driver
// error that looks like the database is down. Errors name the variable and
// never echo the raw value (it holds a password); scheme and host are safe
// to show once the URL has parsed.
//
// Three schemes are supported: postgres and postgresql select the pgx pool,
// sqlite selects a single-file database (no server, for single-node
// deployments). The scheme decides the backend in internal/store, so a typo
// here must not silently pick one.
func validateDatabaseURL(raw string) error {
	const supported = "supported schemes: postgres, postgresql, sqlite"
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("DATABASE_URL is required (e.g. %s, or sqlite:///var/lib/sorobeacon/sorobeacon.db)", databaseURLExample)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("DATABASE_URL is not a parseable URL (%s)", supported)
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
		if u.Host == "" {
			return fmt.Errorf("DATABASE_URL is not a parseable URL (%s)", supported)
		}
		return nil
	case "sqlite":
		if u.Opaque == "" && u.Host == "" && u.Path == "" {
			return fmt.Errorf("DATABASE_URL sqlite URL is missing a database file path (e.g. sqlite:///var/lib/sorobeacon/sorobeacon.db)")
		}
		return nil
	default:
		return fmt.Errorf("DATABASE_URL scheme %q (host %s) is not supported (%s)", u.Scheme, u.Host, supported)
	}
}

// validateReplicaDatabaseURL checks REPLICA_DATABASE_URL. The replica must be
// a Postgres URL — there is no replica concept for SQLite, which
// Load rejects before calling this — and it must not be the primary itself.
// Pointing both at the same string is almost always a half-finished edit, and
// it is worth failing on: it looks like routing is on in every log line and
// every metric label, while every "replica" read lands on the primary. An
// operator who genuinely wants one database behind two pools should say so
// with two URLs.
func validateReplicaDatabaseURL(replicaURL, primaryURL string) error {
	u, err := url.Parse(replicaURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("REPLICA_DATABASE_URL is not a parseable URL (want a postgres connection string, e.g. postgres://user:pass@replica-host:5432/sorobeacon)")
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
	default:
		return fmt.Errorf("REPLICA_DATABASE_URL scheme %q is not supported: reads can only be routed to a Postgres replica", u.Scheme)
	}
	if replicaURL == primaryURL {
		return fmt.Errorf("REPLICA_DATABASE_URL is identical to DATABASE_URL; unset it to read from the primary, or point it at the replica")
	}
	return nil
}

// isSQLiteURL reports whether raw selects the SQLite backend. It is a
// best-effort parse: an unparseable value has already been rejected by
// validateDatabaseURL, so a false here simply means "not sqlite".
func isSQLiteURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, "sqlite")
}

// parseAPITokens splits API_TOKEN on commas into the accepted bearer
// tokens. A list rather than a single value is what makes rotation
// possible without downtime: add the new token, roll clients over, drop the
// old one. Entries are trimmed so a token cannot carry surrounding
// whitespace (the header parser trims too, so both sides agree).
//
// Unset or empty disables authentication. A value that is set but yields no
// usable token — ",", "   ", a stray comma — is an error rather than a
// silent fallback to open access, because the operator clearly meant to
// require a token. Errors never echo the value: it is a credential.
func parseAPITokens(raw string) ([]string, error) {
	var tokens []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	if raw != "" && len(tokens) == 0 {
		return nil, fmt.Errorf("invalid API_TOKEN: set but contains no tokens (use comma-separated values, or unset it to leave authentication off)")
	}
	return tokens, nil
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
