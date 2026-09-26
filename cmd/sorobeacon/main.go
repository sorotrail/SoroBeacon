// Command sorobeacon runs the SoroBeacon monitoring service: the event
// poller, the JSON API and the dashboard, all in one process. Given any
// argument it acts as a CLI for a running instance instead — see cli.go.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/backfill"
	"github.com/sorotrail/sorobeacon/internal/config"
	sorogrpc "github.com/sorotrail/sorobeacon/internal/grpc"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/secrets"
	"github.com/sorotrail/sorobeacon/internal/sorotrail"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/telemetry"
	"github.com/sorotrail/sorobeacon/internal/web"
)

// main dispatches on the arguments. With none, the binary is the monitoring
// service — how the container image and every existing deployment invoke it.
// With any, it is a client for a running instance, so bootstrapping and
// scripted changes stop needing a curl script. An unrecognised command is an
// error rather than a silent server start, because a typo'd subcommand is
// otherwise impossible to notice.
func main() {
	args := os.Args[1:]
	// `sorobeacon backfill` is an opt-in one-shot that replays history for a
	// single monitor; any other argument is a client command against a
	// running instance.
	if len(args) > 0 && args[0] == "backfill" {
		if err := runBackfill(args[1:]); err != nil {
			slog.Error("backfill failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 {
		switch args[0] {
		case "backup":
			if err := runBackup(args[1:]); err != nil {
				slog.Error("backup failed", "err", err)
				os.Exit(1)
			}
			return
		case "restore":
			if err := runRestore(args[1:]); err != nil {
				slog.Error("restore failed", "err", err)
				os.Exit(1)
			}
			return
		}
		if err := runCLI(context.Background(), args, os.Stdout); err != nil {
			reportCLIError(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
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

	// Tracing is entirely off unless OTLP_ENDPOINT is set; with it unset
	// Setup installs a no-op provider, spans cost nothing and Shutdown is
	// a no-op. When enabled, the flush below must run before the process
	// exits or the last spans of a deployment — often the interesting ones
	// — are dropped with the batcher's queue.
	tel, err := telemetry.Setup(ctx, telemetry.Config{
		Endpoint:    cfg.OTLP.Endpoint,
		ServiceName: cfg.OTLP.ServiceName,
		SampleRate:  cfg.OTLP.SampleRate,
	})
	if err != nil {
		return err
	}
	if tel.Enabled() {
		log.Info("opentelemetry tracing enabled",
			"otlp_endpoint", cfg.OTLP.Endpoint,
			"otlp_service_name", cfg.OTLP.ServiceName,
			"otlp_sample_rate", cfg.OTLP.SampleRate)
	}
	defer func() {
		// Bounded so a hung collector delays shutdown by at most this long.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tel.Shutdown(flushCtx); err != nil {
			log.Warn("flush pending spans on shutdown", "error", err)
		}
	}()

	// Storage. The DATABASE_URL scheme selects the backend: postgres /
	// postgresql for the pgx pool, sqlite for a single-file database that
	// removes the Postgres prerequisite on a small VPS or Raspberry Pi. Both
	// implement store.Store and apply their own embedded migrations.
	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	// Metrics are built before the store now, because the store labels every
	// routed read with the pool that served it. Nothing else about the order
	// changes: m is still the same instance the poller, the dispatcher and the
	// HTTP middleware use below.
	m := metrics.New()
	st, err := store.New(ctx, cfg.DatabaseURL, store.PoolSettings{
		MaxConns:        cfg.DatabaseMaxConns,
		MinConns:        cfg.DatabaseMinConns,
		MaxConnLifetime: cfg.DatabaseMaxConnLifetime,
		MaxConnIdleTime: cfg.DatabaseMaxConnIdleTime,
		ReplicaURL:      cfg.ReplicaDatabaseURL,
		Metrics:         m,
	}, configCipher)
	if err != nil {
		return err
	}
	// store.New hands back the Store interface, which deliberately carries no
	// telemetry: a backend without spans, and every test fake, would
	// otherwise have to grow a no-op method for it. Narrow to the backend
	// that does open store.create_alert spans instead; the other backends
	// still contribute their stage to the trace through the poller, rules and
	// dispatcher, they simply add no span of their own here.
	if p, ok := st.(*store.Postgres); ok {
		p.WithTelemetry(tel)
	}
	defer st.Close()
	log.Info("database ready", "backend", store.BackendName(cfg.DatabaseURL))
	if cfg.ReplicaDatabaseURL != "" {
		// The URL itself stays out of the line: it carries credentials. The
		// host is the part an operator checks, and LogAttrs already printed it
		// redacted at startup.
		log.Info("read replica enabled", "routed_reads", "monitors list, alert search, stats, alert counts by day")
	}

	// Postgres partitions alerts by month. Make sure the months just ahead
	// exist before the poller can write into them, so a row never has to fall
	// back to the default partition under normal operation. A no-op on SQLite.
	if pe, ok := st.(store.PartitionEnsurer); ok {
		if err := pe.EnsureAlertPartitions(ctx, time.Now().UTC(), 3); err != nil {
			return err
		}
	}

	// Pipeline: event source -> rules -> alerts -> channels. The source is
	// the single seam between the poller and wherever events come from. The
	// backfill subcommand shares this wiring so a replay reads exactly what
	// live monitoring reads.
	src, health, err := buildSource(ctx, log, cfg)
	if err != nil {
		return err
	}
	logStartupHealth(ctx, log, health)

	registry := rules.NewRegistry()
	// The frequency rule keeps a rolling window per rule in memory. Rebuild it
	// from the alerts already stored so a restart does not forget that the rule
	// fired and alert again for the same episode.
	//
	// This read has to see alerts this process wrote moments ago, so it
	// bypasses replica routing when the store offers a primary-bound reader:
	// a window rebuilt from a replica that is even slightly behind is missing
	// its most recent matches, and the rule would re-fire for an episode it
	// has already alerted on. SQLite does not route reads, so it does not
	// implement PrimaryReader and its ListAlerts is already the primary.
	rebuildAlerts := st.ListAlerts
	if pr, ok := st.(store.PrimaryReader); ok {
		rebuildAlerts = pr.ListAlertsPrimary
	}
	registry.Register(rules.TypeFrequencyThreshold, rules.NewFrequencyThreshold().WithMatchLog(
		rules.MatchLogFunc(func(ctx context.Context, ruleID int64, since time.Time) ([]rules.MatchRecord, error) {
			alerts, err := rebuildAlerts(ctx, store.AlertFilter{RuleID: ruleID, From: since, Sort: "created_at_asc", Limit: 1000})
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
	// External secrets: when a provider is configured, ${secret:...}
	// references in channel configs resolve when a notifier is built. The
	// stored config keeps the reference.
	if resolver := buildSecretResolver(cfg, log); resolver != nil {
		factory.WithSecrets(resolver)
	}
	dispatcher := notify.NewDispatcher(st, factory, log).WithMetrics(m).WithDigestQueue(st)
	// One in-process fan-out carries newly created alerts to the SSE endpoint.
	// The poller publishes into exactly the instance the API serves from, so
	// /alerts/stream needs no database round-trip to show a live alert.
	liveAlerts := broadcast.New(broadcast.DefaultBuffer)
	m.RegisterStreamDropped(liveAlerts.Dropped)
	p := poller.New(src, st, registry, dispatcher, cfg.PollInterval, log).
		WithMetrics(m).
		WithPublisher(liveAlerts).
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
		WithBroadcaster(liveAlerts).
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
	go dispatcher.RunDigestFlusher(ctx, notify.DefaultDigestFlushInterval)
	if cfg.GRPCAddr != "" {
		grpcSrv := sorogrpc.New(st, authn, log)
		go func() {
			if err := grpcSrv.Serve(ctx, cfg.GRPCAddr); err != nil {
				log.Error("grpc server error", "err", err)
			}
		}()
	}
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

// buildSecretResolver selects the external-secret provider named by
// SECRETS_PROVIDER. It returns nil when external secrets are disabled, so
// the factory treats ${secret:...} strings as literals exactly as it did
// before the feature. The Vault token is a credential and is never logged.
func buildSecretResolver(cfg config.Config, log *slog.Logger) *secrets.Resolver {
	switch cfg.SecretsProvider {
	case "env":
		log.Info("external secrets enabled", "secrets_provider", "env", "cache_ttl", cfg.SecretsCacheTTL.String())
		return secrets.NewResolver(secrets.NewEnvProvider()).WithTTL(cfg.SecretsCacheTTL)
	case "vault":
		log.Info("external secrets enabled", "secrets_provider", "vault", "vault_addr", cfg.VaultAddr, "cache_ttl", cfg.SecretsCacheTTL.String())
		return secrets.NewResolver(secrets.NewVaultProvider(cfg.VaultAddr, cfg.VaultToken, cfg.VaultNamespace)).WithTTL(cfg.SecretsCacheTTL)
	default:
		return nil
	}
}

// buildSource wires the configured event source and its health checker. It is
// shared by the long-lived server and the backfill subcommand so both honour
// SOURCE_MODE and talk to the same backend.
func buildSource(ctx context.Context, log *slog.Logger, cfg config.Config) (poller.EventSource, api.HealthChecker, error) {
	switch cfg.SourceMode {
	case "sorotrail":
		stc := sorotrail.NewClient(cfg.SoroTrailURL, nil)
		log.Info("upstream mode: reading events from SoroTrail", "url", cfg.SoroTrailURL)
		return sorotrail.NewSource(stc), stc, nil
	default: // "rpc"
		// Several endpoints behind one Client: calls try them in the order
		// RPC_URLS lists them and fail over when one rate-limits or goes
		// down. The poller, the spec source and the readiness probe all
		// keep talking to a single stellar.Client, so nothing downstream
		// knows the difference.
		rpc := stellar.NewFailoverClient(cfg.RPCURLs, nil, log)

		// Verify every RPC endpoint really is the configured network before
		// any monitor starts evaluating events. A mainnet endpoint behind a
		// testnet config (or the reverse) silently evaluates every rule
		// against the wrong chain, and because failover picks a node per
		// call, one mixed endpoint would corrupt the alert stream
		// intermittently — the hardest kind of bug to notice. This fails
		// fast instead. There is no equivalent check in upstream mode: the
		// indexer's own deployment owns its network.
		if err := verifyNetworkEndpoints(ctx, log, rpc, cfg.Network.Passphrase); err != nil {
			return nil, nil, err
		}
		log.Info("network verified",
			"network", cfg.Network.Name,
			"rpc_url", cfg.RPCURL,
			"rpc_endpoint_count", len(cfg.RPCURLs))

		// Contract specs are fetched lazily per contract and cached, so
		// events from a contract with a spec arrive with named fields while
		// every other contract decodes exactly as before.
		decoder := stellar.NewSpecDecoder(stellar.DefaultDecoder{}, stellar.NewRPCSpecSource(rpc), log)
		return poller.NewRPCSource(rpc, decoder), rpc, nil
	}
}

// runBackfill implements `sorobeacon backfill`: an opt-in historical replay of
// one monitor's recent ledger history. It shares the server's config, store and
// event source, so a replay reads exactly what live monitoring reads.
func runBackfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	monitorID := fs.Int64("monitor", 0, "monitor id to backfill (required)")
	fromLedger := fs.Uint("from", 0, "inclusive first ledger to replay")
	toLedger := fs.Uint("to", 0, "inclusive last ledger to replay (default: source tip)")
	lookback := fs.Duration("lookback", 0, "replay this far back from the end of the range, e.g. 72h (estimate; ignored when -from is set)")
	deliver := fs.Bool("deliver", false, "send backfilled alerts to the monitor's channels (default: store only)")
	rate := fs.Duration("rate", 0, "minimum delay between page fetches (default 200ms)")
	pageSize := fs.Int("page-size", 0, "getEvents page size (default: the source's)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h/-help already printed usage
		}
		return err
	}
	if *monitorID == 0 {
		return errors.New("backfill: -monitor is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	st, err := store.NewPostgres(ctx, cfg.DatabaseURL, store.PoolSettings{
		MaxConns:        cfg.DatabaseMaxConns,
		MinConns:        cfg.DatabaseMinConns,
		MaxConnLifetime: cfg.DatabaseMaxConnLifetime,
		MaxConnIdleTime: cfg.DatabaseMaxConnIdleTime,
	})
	if err != nil {
		return err
	}
	defer st.Close()
	// Channel configs hold secrets and may be encrypted at rest; decrypt them
	// so the dispatcher can use them when -deliver is set.
	if len(cfg.ConfigEncryptionKey) > 0 {
		cipher, err := store.NewAESGCMCipher(cfg.ConfigEncryptionKey)
		if err != nil {
			return err
		}
		st.WithConfigCipher(cipher)
	}

	src, _, err := buildSource(ctx, log, cfg)
	if err != nil {
		return err
	}

	registry := rules.NewRegistry()
	dispatcher := notify.NewDispatcher(st, notify.DefaultFactory(), log)
	ingestor := poller.NewIngestor(st, registry, dispatcher, log)
	job := backfill.New(src, st, registry, ingestor, log)

	res, err := job.Run(ctx, backfill.Options{
		MonitorID:  *monitorID,
		FromLedger: uint32(*fromLedger),
		ToLedger:   uint32(*toLedger),
		Lookback:   *lookback,
		Deliver:    *deliver,
		Rate:       *rate,
		PageSize:   *pageSize,
	})
	if err != nil {
		return err
	}
	log.Info("backfill finished",
		"monitor_id", res.MonitorID,
		"from_ledger", res.FromLedger,
		"to_ledger", res.ToLedger,
		"oldest_ledger", res.OldestLedger,
		"clamped", res.Clamped,
		"resumed", res.Resumed,
		"events", res.Events,
		"matched", res.Matched,
		"alerts", res.Alerts,
		"dispatched", res.Dispatched,
	)
	return nil
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

// startupNetworkTimeout bounds the startup passphrase sweep across every
// configured endpoint, so a set of unreachable endpoints delays boot by at
// most this long rather than once per endpoint's own HTTP timeout.
const startupNetworkTimeout = 15 * time.Second

// verifyNetworkEndpoints asks every configured RPC endpoint which network it
// belongs to and refuses to start if any of them reports a passphrase other
// than the configured one. Failover spreads requests over the whole list, so
// a list that spans two networks would emit a mixture of events from both
// chains — this is the check that stops it.
//
// An endpoint that cannot be reached is logged and skipped rather than fatal:
// unreachability is precisely what failover exists for, and an endpoint that
// is down at boot may well be the healthy one a minute later. Only a wrong
// answer is fatal.
func verifyNetworkEndpoints(ctx context.Context, log *slog.Logger, client *stellar.FailoverClient, configured string) error {
	ctx, cancel := context.WithTimeout(ctx, startupNetworkTimeout)
	defer cancel()

	for _, ep := range client.Endpoints() {
		net, err := ep.Client.GetNetwork(ctx)
		if err != nil {
			log.Warn("could not verify network passphrase", "rpc_url", ep.URL, "error", err)
			continue
		}
		if err := config.VerifyPassphrase(configured, net.Passphrase); err != nil {
			return fmt.Errorf("rpc endpoint %s: %w", ep.URL, err)
		}
	}
	return nil
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
