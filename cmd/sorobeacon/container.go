// Self-contained container helper subcommands (`healthcheck` and `migrate`) so
// the runtime container image can be built FROM scratch with no shell (/bin/sh)
// and no external network utilities (wget, curl).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/store"
)

const healthcheckUsage = `sorobeacon healthcheck — probe a local SoroBeacon HTTP health endpoint.

Usage:
  sorobeacon healthcheck [--url URL] [--addr ADDR] [--path PATH] [--timeout DURATION]

Built into the binary so a minimal scratch/distroless container image can run
Docker and Compose HEALTHCHECK probes without shipping /bin/sh, wget, or curl.

Flags:
  --url URL        explicit probe URL (overrides --addr and --path)
  --addr ADDR      listen address to derive loopback target from (default $HTTP_ADDR or :8080)
  --path PATH      HTTP path to probe (default /api/v1/health)
  --timeout DUR    request timeout (default 3s)
  --insecure       skip TLS certificate verification on loopback HTTPS probes
`

const migrateUsage = `sorobeacon migrate — apply embedded database migrations and verify schema state.

Usage:
  sorobeacon migrate [--attempts N] [--retry-delay DURATION] [--timeout DURATION] [--probe-path PATH]

Applies all embedded database migrations and verifies that the schema version
matches the binary's latest expected version and is not dirty. Built into the
binary so the Helm pre-install/pre-upgrade migration hook Job can run in a
shell-less scratch container without starting the full HTTP server or relying
on /bin/sh and wget.

Flags:
  --attempts N         maximum migration attempts before failing (default 3)
  --retry-delay DUR    wait time between failed attempts (default 2s)
  --timeout DUR        per-attempt verification timeout (default 30s)
  --probe-path PATH    probe endpoint contract identifier (default /api/v1/livez)
`

// runHealthcheck probes the local instance's health endpoint and returns nil
// on any 2xx response.
func runHealthcheck(ctx context.Context, args []string, out io.Writer) error {
	fs := newFlagSet("healthcheck")
	var (
		explicitURL string
		addr        string
		probePath   string
		timeout     time.Duration
		insecure    bool
	)
	defaultTLS := strings.TrimSpace(os.Getenv("TLS_CERT_FILE")) != "" && strings.TrimSpace(os.Getenv("TLS_KEY_FILE")) != ""
	fs.StringVar(&explicitURL, "url", "", "explicit probe URL")
	fs.StringVar(&addr, "addr", "", "listen address override (defaults to $HTTP_ADDR or :8080)")
	fs.StringVar(&probePath, "path", "/api/v1/health", "probe HTTP path")
	fs.DurationVar(&timeout, "timeout", 3*time.Second, "probe timeout")
	fs.BoolVar(&insecure, "insecure", defaultTLS, "skip TLS verification on loopback HTTPS probe")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, healthcheckUsage)
			return nil
		}
		return err
	}
	if err := noExtraArgs(fs); err != nil {
		return err
	}
	if timeout <= 0 {
		return errors.New("timeout must be positive")
	}

	effectiveAddr := firstNonEmpty(strings.TrimSpace(addr), strings.TrimSpace(os.Getenv("HTTP_ADDR")), ":8080")
	targetURL, err := resolveHealthcheckURL(strings.TrimSpace(explicitURL), effectiveAddr, strings.TrimSpace(probePath), defaultTLS)
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return fmt.Errorf("build healthcheck request: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		// Loopback container healthcheck against a self-signed or cluster-internal
		// server certificate only verifies liveness of the local listener.
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} // #nosec G402 -- intentional loopback healthcheck probe option
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck probe failed: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("healthcheck probe returned status %d", resp.StatusCode)
	}
	return nil
}

// resolveHealthcheckURL constructs the loopback URL to probe when --url is not
// explicitly supplied.
func resolveHealthcheckURL(explicitURL, addr, probePath string, tlsEnabled bool) (string, error) {
	if explicitURL != "" {
		u, err := url.Parse(explicitURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("invalid healthcheck url %q", explicitURL)
		}
		return u.String(), nil
	}

	if probePath == "" {
		probePath = "/api/v1/health"
	}
	if !strings.HasPrefix(probePath, "/") {
		probePath = "/" + probePath
	}

	host := "127.0.0.1"
	var port string
	if strings.HasPrefix(addr, ":") {
		port = strings.TrimPrefix(addr, ":")
		if port == "" {
			return "", fmt.Errorf("invalid listen address %q", addr)
		}
	} else {
		h, p, err := net.SplitHostPort(addr)
		if err != nil || p == "" {
			return "", fmt.Errorf("invalid listen address %q", addr)
		}
		port = p
		switch h {
		case "", "0.0.0.0", "::", "[::]":
			host = "127.0.0.1"
		default:
			host = h
		}
	}

	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, port), probePath), nil
}

type migrateDeps struct {
	migrate        func(string) error
	migrationState func(context.Context, string) (uint, uint, bool, error)
}

func defaultMigrateDeps() migrateDeps {
	return migrateDeps{
		migrate:        store.Migrate,
		migrationState: store.MigrationState,
	}
}

// runMigrate applies embedded database migrations and verifies that the schema
// is up to date and clean.
func runMigrate(ctx context.Context, args []string, out io.Writer) error {
	return runMigrateWith(ctx, args, out, defaultMigrateDeps())
}

func runMigrateWith(ctx context.Context, args []string, out io.Writer, deps migrateDeps) error {
	fs := newFlagSet("migrate")
	var (
		attempts   int
		retryDelay time.Duration
		timeout    time.Duration
		probePath  string
	)
	fs.IntVar(&attempts, "attempts", 3, "maximum migration attempts")
	fs.DurationVar(&retryDelay, "retry-delay", 2*time.Second, "delay between attempts")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "per-attempt verification timeout")
	fs.StringVar(&probePath, "probe-path", "/api/v1/livez", "readiness contract path")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, migrateUsage)
			return nil
		}
		return err
	}
	if err := noExtraArgs(fs); err != nil {
		return err
	}
	if attempts < 1 {
		return errors.New("attempts must be at least 1")
	}
	if timeout <= 0 {
		return errors.New("timeout must be positive")
	}

	dbURL, cfg, err := resolveMigrateDatabaseURL()
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		lastErr = applyAndVerifyMigrations(ctx, dbURL, timeout, deps)
		if lastErr == nil {
			_, _ = fmt.Fprintln(out, "sorobeacon: schema is up to date")
			return nil
		}
		if attempt < attempts {
			if retryDelay > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(retryDelay):
				}
			}
		}
	}
	return fmt.Errorf("migrations did not complete after %d attempt(s): %s", attempts, redactSecrets(lastErr.Error(), cfg))
}

func resolveMigrateDatabaseURL() (string, config.Config, error) {
	cfg, _ := config.LoadProblems()
	dbURL := strings.TrimSpace(cfg.DatabaseURL)
	if dbURL == "" {
		dbURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
		cfg.DatabaseURL = dbURL
	}
	if dbURL == "" {
		return "", cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.DatabaseTLSEnabled() {
		withTLS, err := store.ApplyDatabaseTLS(dbURL, cfg.DatabaseSSLMode, cfg.DatabaseSSLCertFile, cfg.DatabaseSSLKeyFile, cfg.DatabaseSSLRootCertFile)
		if err != nil {
			return "", cfg, fmt.Errorf("apply database TLS: %s", redactSecrets(err.Error(), cfg))
		}
		dbURL = withTLS
	}
	return dbURL, cfg, nil
}

func applyAndVerifyMigrations(ctx context.Context, dbURL string, timeout time.Duration, deps migrateDeps) error {
	if err := deps.migrate(dbURL); err != nil {
		return err
	}
	stateCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	current, latest, dirty, err := deps.migrationState(stateCtx, dbURL)
	if err != nil {
		return fmt.Errorf("verify database migration state: %w", err)
	}
	if dirty || current != latest {
		return fmt.Errorf("verify database migration state: current=%d latest=%d dirty=%t", current, latest, dirty)
	}
	return nil
}
