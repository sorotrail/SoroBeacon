// Command sorobeacon runs the SoroBeacon monitoring service: the event
// poller, the JSON API and the dashboard, all in one process.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sorotrail/sorobeacon/internal/api"
	"github.com/sorotrail/sorobeacon/internal/archive"
	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/sorotrail"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.LogAttrs(context.Background(), slog.LevelInfo, "configuration loaded", cfg.LogAttrs()...)

	// Channel config holds secrets; encrypt it at rest when a key is set.
	// The key is validated here so a malformed value fails startup rather
	// than the first channel write. With no key the store keeps storing
	// plaintext (unchanged behaviour) and we warn once below.
	var configCipher store.ConfigCipher
	if len(cfg.ConfigEncryptionKey) > 0 {
		configCipher, err = store.NewAESGCMCipher(cfg.ConfigEncryptionKey)
		if err != nil {
			return err
		}
	}
	warnIfChannelConfigUnencrypted(log, cfg.ConfigEncryptionKey)

	// Authentication. One authenticator is shared by the JSON API (bearer
	// token) and the dashboard (a session cookie minted from the same
	// tokens) so a single API_TOKEN covers both, and a session established
	// at /login also satisfies the API middleware — the dashboard links
	// straight to /api/v1/alerts.csv, which a browser fetches without
	// headers. The tokens themselves are never logged.
	authn := auth.New(cfg.APITokens, auth.DefaultSessionTTL)
	warnIfAPITokenUnset(log, cfg.APITokens)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Storage. The DATABASE_URL scheme selects the backend: postgres /
	// postgresql for the pgx pool, sqlite for a single-file database that
	// removes the Postgres prerequisite on a small VPS or Raspberry Pi. Both
	// implement store.Store and apply their own embedded migrations.
	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	st, err := store.New(ctx, cfg.DatabaseURL, store.PoolSettings{
		MaxConns:        cfg.DatabaseMaxConns,
		MinConns:        cfg.DatabaseMinConns,
		MaxConnLifetime: cfg.DatabaseMaxConnLifetime,
		MaxConnIdleTime: cfg.DatabaseMaxConnIdleTime,
	}, configCipher)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Info("database ready", "backend", store.BackendName(cfg.DatabaseURL))

	// Postgres partitions alerts by month. Make sure the months just ahead
	// exist before the poller can write into them, so a row never has to fall
	// back to the default partition under normal operation. A no-op on SQLite.
	if pe, ok := st.(store.PartitionEnsurer); ok {
		if err := pe.EnsureAlertPartitions(ctx, time.Now().UTC(), 3); err != nil {
			return err
		}
	}

	// Pipeline: event source -> rules -> alerts -> channels. The source is
	// the single seam between the poller and wherever events come from.
	var src poller.EventSource
	var health api.HealthChecker
	switch cfg.SourceMode {
	case "sorotrail":
		stc := sorotrail.NewClient(cfg.SoroTrailURL, nil)
		src = sorotrail.NewSource(stc)
		health = stc
		log.Info("upstream mode: reading events from SoroTrail", "url", cfg.SoroTrailURL)
	default: // "rpc"
		rpc := stellar.NewHTTPClient(cfg.RPCURL, nil)

		// Verify the RPC endpoint really is the configured network before
		// any monitor starts evaluating events. A mainnet endpoint behind
		// a testnet config (or the reverse) silently evaluates every rule
		// against the wrong chain — this fails fast instead. There is no
		// equivalent check in upstream mode: the indexer's own deployment
		// owns its network.
		if net, err := rpc.GetNetwork(ctx); err != nil {
			log.Warn("could not verify network passphrase", "error", err)
		} else if err := config.VerifyPassphrase(cfg.Network.Passphrase, net.Passphrase); err != nil {
			return err
		}
		log.Info("network verified", "network", cfg.Network.Name, "rpc_url", cfg.RPCURL)

		// Contract specs are fetched lazily per contract and cached, so
		// events from a contract with a spec arrive with named fields while
		// every other contract decodes exactly as before.
		decoder := stellar.NewSpecDecoder(stellar.DefaultDecoder{}, stellar.NewRPCSpecSource(rpc), log)
		src = poller.NewRPCSource(rpc, decoder)
		health = rpc
	}
	logStartupHealth(ctx, log, health)

	m := metrics.New()
	registry := rules.NewRegistry()
	// The frequency rule keeps a rolling window per rule in memory. Rebuild it
	// from the alerts already stored so a restart does not forget that the rule
	// fired and alert again for the same episode.
	registry.Register(rules.TypeFrequencyThreshold, rules.NewFrequencyThreshold().WithMatchLog(
		rules.MatchLogFunc(func(ctx context.Context, ruleID int64, since time.Time) ([]rules.MatchRecord, error) {
			alerts, err := st.ListAlerts(ctx, store.AlertFilter{RuleID: ruleID, From: since, Sort: "created_at_asc", Limit: 1000})
			if err != nil {
				return nil, err
			}
			records := make([]rules.MatchRecord, 0, len(alerts))
			for _, a := range alerts {
				records = append(records, rules.MatchRecord{At: a.CreatedAt, EventID: a.EventID})
			}
			return records, nil
		})))
	factory := notify.DefaultFactory()
	// Channel health: delivery outcomes are folded into each channel so a
	// revoked token surfaces as a broken channel instead of as silence. The
	// threshold is off unless the operator sets it — auto-disabling a channel
	// is a destructive answer to a temporary problem.
	dispatcher := notify.NewDispatcher(st, factory, log).
		WithMetrics(m).
		WithDisableAfterFailures(cfg.ChannelDisableAfterFailures)
	p := poller.New(src, st, registry, dispatcher, cfg.PollInterval, log).WithMetrics(m)
	dispatcher := notify.NewDispatcher(st, factory, log).WithMetrics(m)
	p := poller.New(src, st, registry, dispatcher, cfg.PollInterval, log).
		WithMetrics(m).
		WithReorg(cfg.ReorgTrackingWindow, cfg.ReorgConfirmationDepth)

	// HTTP: JSON API under /api/v1, dashboard at /.
	apiSrv := api.New(st, registry, factory, health, log).
		WithPoller(p).
		WithReadyzLagThreshold(cfg.ReadyzLagThreshold).
		WithRateLimit(api.RateLimitConfig{
			RPS:            cfg.RateLimitRPS,
			Burst:          cfg.RateLimitBurst,
			TrustForwarded: cfg.RateLimitTrustForwarded,
		}).
		WithMaxBodyBytes(cfg.HTTPMaxBodyBytes).
		WithAuth(authn)
	webSrv, err := web.New(st, registry, factory, log)
	if err != nil {
		return err
	}
	webSrv.WithPoller(p).WithSilentAfter(cfg.MonitorSilentAfter).WithAuth(authn)
	root := chi.NewRouter()
	// RequestLog must sit outside Recoverer so a panic still emits the
	// access line after chi writes 500. reqid first so the line can
	// carry the correlation id.
	root.Use(reqid.Middleware, api.RequestLog(log), middleware.Recoverer)
	root.Use(api.CORSMiddleware(api.CORSConfig{Origins: cfg.CORSAllowedOrigins}))
	// RoutePattern returns the matched chi pattern (e.g. "/api/v1/monitors/{id}")
	// rather than the raw path, keeping metric label cardinality bounded.
	metrics.RoutePattern = func(r *http.Request) string {
		if rc := chi.RouteContext(r.Context()); rc != nil {
			return rc.RoutePattern()
		}
		return ""
	}
	root.Use(m.Middleware)
	root.Handle("/metrics", m.Handler())
	root.Mount("/api/v1", apiSrv.Routes())
	root.Mount("/", webSrv.Routes())

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr, "rpc_url", cfg.RPCURL)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go p.Run(ctx)
	// Retention can tier expired alerts to object storage before deleting
	// them. Off by default: an empty ARCHIVE_URL leaves the pruner behaving
	// exactly as it did before archiving existed.
	var archiver store.AlertArchiver
	if cfg.ArchiveURL != "" {
		back, err := archive.FromURL(cfg.ArchiveURL)
		if err != nil {
			return err
		}
		pruner, err := archive.NewPruner(back)
		if err != nil {
			return err
		}
		archiver = pruner
		// Never log the URL: an operator may embed an endpoint or token in a
		// query string. The scheme is enough to confirm what was selected.
		log.Info("alert archiving enabled")
	}
	if cfg.AlertRetention > 0 {
		go store.RunAlertPruner(ctx, st, cfg.AlertRetention, store.DefaultPruneInterval, store.DefaultPruneBatch, archiver, log)
	} else if archiver != nil {
		// Archiving only happens before a delete, so it is inert without
		// retention. Warn rather than silently doing nothing.
		log.Warn("ARCHIVE_URL is set but ALERT_RETENTION is unset; nothing will be archived or deleted")
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// warnIfChannelConfigUnencrypted logs one warning at startup when
// CONFIG_ENCRYPTION_KEY is unset. Channel configs still work as plaintext, so
// this is a warning and not a startup failure — an upgrade must never brick a
// running deployment — but the operator should know that anyone with
// database or backup access can read the webhook URLs, bot tokens and SMTP
// credentials those configs hold.
func warnIfChannelConfigUnencrypted(log *slog.Logger, key []byte) {
	if len(key) == 0 {
		log.Warn("channel config encryption is disabled; set CONFIG_ENCRYPTION_KEY to encrypt webhook URLs, bot tokens and SMTP credentials at rest")
	}
}

// warnIfAPITokenUnset logs one warning at startup when API_TOKEN is unset.
// Both the API and the dashboard stay open, which is how the docker-compose
// quickstart and every existing deployment behave — so this is a warning and
// not a startup failure. The operator should still know: an unauthenticated
// API can create, rewrite and delete monitors and channels from anywhere the
// port is reachable.
func warnIfAPITokenUnset(log *slog.Logger, tokens []string) {
	if len(tokens) == 0 {
		log.Warn("API authentication is disabled; set API_TOKEN to require a bearer token on /api/v1 and a sign-in on the dashboard")
	}
}

// startupHealthTimeout bounds the one-off health check logged at startup,
// so a slow or unreachable dependency delays boot by at most this long
// rather than hanging it.
const startupHealthTimeout = 5 * time.Second

// logStartupHealth reports the event source's health once at startup, so a
// misconfigured RPC_URL (or SOROTRAIL_URL, in upstream mode) or an
// unreachable node is visible immediately instead of silently surfacing on
// the first poll failure. It never fails startup: the poller already
// retries with backoff, so an unhealthy source at boot is logged as a
// warning and left to recover on its own.
func logStartupHealth(ctx context.Context, log *slog.Logger, health api.HealthChecker) {
	ctx, cancel := context.WithTimeout(ctx, startupHealthTimeout)
	defer cancel()
	h, err := health.GetHealth(ctx)
	if err != nil {
		log.Warn("event source health check failed at startup", "error", err)
		return
	}
	log.Info("event source healthy", "status", h.Status, "latest_ledger", h.LatestLedger)
}
