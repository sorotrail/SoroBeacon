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
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sorotrail/sorobeacon/internal/alerts"
	"github.com/sorotrail/sorobeacon/internal/api"
	"github.com/sorotrail/sorobeacon/internal/api/graphql"
	"github.com/sorotrail/sorobeacon/internal/archive"
	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/backfill"
	"github.com/sorotrail/sorobeacon/internal/broadcast"
	"github.com/sorotrail/sorobeacon/internal/config"
	sorogrpc "github.com/sorotrail/sorobeacon/internal/grpc"
	"github.com/sorotrail/sorobeacon/internal/horizon"
	"github.com/sorotrail/sorobeacon/internal/lease"
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
	"github.com/sorotrail/sorobeacon/internal/workspace"
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
	if len(args) > 0 && args[0] == "preflight" {
		if err := runPreflightCLI(context.Background(), args[1:], os.Stdout); err != nil {
			reportCLIError(os.Stderr, err)
			os.Exit(exitCodeFor(err))
		}
		return
	}
	if len(args) > 0 {
		switch args[0] {
		case "backup":
			if err := runBackup(args[1:]); err != nil {
				slog.Error("backup failed", "err", err)
				os.Exit(exitCodeFor(err))
			}
			return
		case "restore":
			if err := runRestore(args[1:]); err != nil {
				slog.Error("restore failed", "err", err)
				os.Exit(exitCodeFor(err))
			}
			return
		}
		// `sorobeacon validate` checks the configuration and exits with a
		// code a deployment pipeline can branch on, so it is dispatched
		// before the client: it talks to no instance and starts no server.
		if ok, rest := validateRequested(args); ok {
			os.Exit(runValidate(context.Background(), rest, os.Stdout))
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
	// The handler reads its minimum level through a LevelVar — an atomic
	// under the hood — so a SIGHUP reload can raise verbosity on a running
	// process without rebuilding the logger every goroutine already holds.
	var level slog.LevelVar
	level.Set(cfg.LogLevel)
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &level}))
	slog.SetDefault(log)
	log.LogAttrs(context.Background(), slog.LevelInfo, "configuration loaded", cfg.LogAttrs()...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := newServer(ctx, cfg, log, defaultStartupDeps())
	if err != nil {
		return err
	}
	defer srv.Close()

	// SIGHUP means "reload the settings that can change at runtime", not
	// "exit": shutdown keeps its SIGINT/SIGTERM from the NotifyContext above.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go reloadLoop(ctx, sigs, config.NewReloader(cfg), &level, srv.poller, log)

	return srv.run(ctx)
}

// reloadLoop applies one reload per SIGHUP until ctx is cancelled. It is a
// loop rather than a one-shot handler so a burst of signals is drained one
// reload at a time instead of being collapsed by the signal coalescing a
// channel-based Notify performs.
func reloadLoop(ctx context.Context, sigs <-chan os.Signal, reloader *config.Reloader, level *slog.LevelVar, p *poller.Poller, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
			applyReload(reloader, level, p, log)
		}
	}
}

// applyReload runs one SIGHUP-triggered reload: re-read and validate the
// configuration, then apply the settings that are safe to change at runtime
// and log what changed, old value to new value. A rejected reload logs the
// reasons and changes nothing — a bad reload must never leave the process
// half-configured.
func applyReload(reloader *config.Reloader, level *slog.LevelVar, p *poller.Poller, log *slog.Logger) {
	changed, err := reloader.Reload()
	if err != nil {
		log.Error("configuration reload rejected", "err", err)
		return
	}
	// The reload applied the reloadable settings to its own copy only after
	// the whole configuration validated, so reading them back here cannot
	// observe a half-applied reload. Consumers see the new values on their
	// next read: the handler on its next record, the poller on its next
	// cycle.
	cfg := reloader.Current()
	level.Set(cfg.LogLevel)
	p.SetInterval(cfg.PollInterval)
	for _, c := range changed.Applied {
		log.Info("configuration reloaded", "setting", c.Setting, "old", c.Old, "new", c.New)
	}
	// Skipped settings did change but need a restart; saying so beats an
	// operator sending SIGHUP repeatedly and wondering why nothing happened.
	for _, c := range changed.Skipped {
		log.Warn("reload skipped a changed setting; it takes effect after a restart", "setting", c.Setting, "old", c.Old, "new", c.New)
	}
	if len(changed.Applied) == 0 && len(changed.Skipped) == 0 {
		log.Info("configuration reload: nothing changed")
	}
}

// leaderLease is the part of lease.Lease the startup path drives: the loop
// that runs the poller while this instance is leader, and the status the API
// and the dashboard render. It is an interface so the startup tests can
// replace the election with a fake and still assert that shutdown waits for
// the lease to be given up.
type leaderLease interface {
	Run(context.Context, func(context.Context))
	Status() lease.Status
}

// startupDeps are the two seams that reach outside the process: where events
// come from, and how leadership is elected. Everything else is built from
// config, so a startup test replaces only these and still exercises the real
// wiring. buildSource receives the opened store so a test can pin that
// migrations ran before the first component that queries it was wired.
type startupDeps struct {
	buildSource func(context.Context, *slog.Logger, config.Config, store.Store) (poller.EventSource, api.HealthChecker, error)
	newLeader   func(config.Config, *slog.Logger) (leaderLease, error)
}

func defaultStartupDeps() startupDeps {
	return startupDeps{
		buildSource: func(ctx context.Context, log *slog.Logger, cfg config.Config, _ store.Store) (poller.EventSource, api.HealthChecker, error) {
			return buildSource(ctx, log, cfg)
		},
		newLeader: func(cfg config.Config, log *slog.Logger) (leaderLease, error) {
			// Leadership. Every instance serves the API and the dashboard, but
			// only the one holding the lease runs the ingest loop: two pollers
			// ingest the same events and race the same checkpoint, so alerts
			// arrive twice and each instance believes the other's progress is
			// its own. Postgres supplies the election as a session advisory
			// lock, which needs no table and no migration. A SQLite database
			// cannot be shared between machines and has no advisory locks, so
			// a SQLite deployment is always the poller.
			//
			// Failing to build the lease is fatal rather than a silent fall
			// back to "everyone polls", which is the bug this prevents.
			if store.BackendName(cfg.DatabaseURL) == "postgres" {
				return lease.NewPostgres(cfg.DatabaseURL, lease.Options{}, log)
			}
			return lease.SingleNode(log), nil
		},
	}
}

// server is everything newServer constructs, held together so run can start
// it and Close can release it. Building it starts no loops: only run does
// that, which is what lets a startup test assert what was wired before any
// goroutine exists.
type server struct {
	cfg config.Config
	log *slog.Logger

	store      store.Store
	poller     *poller.Poller
	dispatcher *notify.Dispatcher
	leader     leaderLease
	archiver   store.AlertArchiver
	tel        *telemetry.Provider
	// heartbeat is the external dead-man's switch, non-nil only when
	// HEARTBEAT_URL is configured. It runs inside job so only the leader
	// pings.
	heartbeat *poller.Heartbeat

	httpSrv      *http.Server
	grpcSrv      *sorogrpc.Server
	httpListener net.Listener
	grpcListener net.Listener

	// Wiring decisions, set once here and used by run. Keeping them in one
	// place means the branch that constructs a component and the loop that
	// starts it cannot disagree about whether it is enabled.
	prunerEnabled   bool
	playground      bool
	shutdownTimeout time.Duration
}

// newServer builds the process from configuration: store and migrations,
// lease, event source, pipeline, HTTP and gRPC servers, and finally the
// listeners they will serve on. It starts nothing. Listeners are bound last,
// so a failure earlier leaves no port held and the deferred Close has nothing
// but the store and tracer to release.
func newServer(ctx context.Context, cfg config.Config, log *slog.Logger, deps startupDeps) (_ *server, err error) {
	// Channel config holds secrets; encrypt it at rest when a key is set.
	// The key is validated here so a malformed value fails startup rather
	// than the first channel write. With no key the store keeps storing
	// plaintext (unchanged behaviour) and we warn once below.
	var configCipher store.ConfigCipher
	if len(cfg.ConfigEncryptionKey) > 0 {
		configCipher, err = store.NewAESGCMCipher(cfg.ConfigEncryptionKey)
		if err != nil {
			return nil, err
		}
	}
	warnIfChannelConfigUnencrypted(log, cfg.ConfigEncryptionKey)

	// Authentication. One authenticator is shared by the JSON API (bearer
	// token) and the dashboard (a session cookie minted from the same
	// tokens) so a single API_TOKEN covers both, and a session established
	// at /login also satisfies the API middleware — the dashboard links
	// straight to /api/v1/alerts.csv, which a browser fetches without
	// headers. The tokens themselves are never logged.
	//
	// Each credential also carries the workspace it acts on: API_TOKEN
	// entries act on the default workspace and WORKSPACE_TOKENS entries on
	// their own, which is how one instance serves several teams without any
	// of them being able to ask for someone else's data.
	bindings, err := cfg.AuthBindings()
	if err != nil {
		return nil, err
	}
	authn := auth.NewBound(bindings, auth.DefaultSessionTTL)

	// Single sign-on, when a provider is configured. Discovery happens here, at
	// startup, rather than on the first visitor's click: a mistyped
	// OIDC_ISSUER, a client id the provider does not know or a provider that is
	// down is a failure in the deploy log, which is where an operator is
	// looking, and not a colleague unable to sign in three days later.
	if cfg.OIDC.Enabled() {
		discoverCtx, cancel := context.WithTimeout(ctx, startupNetworkTimeout)
		defer cancel()
		provider, err := auth.NewOIDCProvider(discoverCtx, cfg.OIDC.AuthOIDC())
		if err != nil {
			return nil, err
		}
		authn.WithOIDC(provider)
		// The issuer URL is configuration, not a secret; the client secret is
		// not named here and never is.
		log.Info("single sign-on enabled", "issuer", cfg.OIDC.Issuer,
			"workspace", cfg.OIDC.Workspace, "allowed_domains", len(cfg.OIDC.AllowedDomains))
	}
	warnIfAuthDisabled(log, authn.Enabled())

	// Tracing is entirely off unless OTLP_ENDPOINT is set; with it unset
	// Setup installs a no-op provider, spans cost nothing and Shutdown is
	// a no-op. When enabled, the flush must run before the process exits or
	// the last spans of a deployment — often the interesting ones — are
	// dropped with the batcher's queue.
	tel, err := telemetry.Setup(ctx, telemetry.Config{
		Endpoint:    cfg.OTLP.Endpoint,
		ServiceName: cfg.OTLP.ServiceName,
		SampleRate:  cfg.OTLP.SampleRate,
	})
	if err != nil {
		return nil, err
	}
	if tel.Enabled() {
		log.Info("opentelemetry tracing enabled",
			"otlp_endpoint", cfg.OTLP.Endpoint,
			"otlp_service_name", cfg.OTLP.ServiceName,
			"otlp_sample_rate", cfg.OTLP.SampleRate)
	}

	// Storage. The DATABASE_URL scheme selects the backend: postgres /
	// postgresql for the pgx pool, sqlite for a single-file database that
	// removes the Postgres prerequisite on a small VPS or Raspberry Pi. Both
	// implement store.Store and apply their own embedded migrations.
	//
	// Ordering that matters: migrations run before anything queries the
	// database, and the store is opened before the lease, the source or any
	// server gets a handle to it. A server wired against a database that has
	// not been migrated would serve 500s until the operator noticed.
	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		shutdownTelemetry(log, tel)
		return nil, err
	}
	stateCtx, cancelState := context.WithTimeout(ctx, startupNetworkTimeout)
	currentMigration, latestMigration, dirtyMigration, err := store.MigrationState(stateCtx, cfg.DatabaseURL)
	cancelState()
	if err != nil {
		shutdownTelemetry(log, tel)
		return nil, fmt.Errorf("verify database migration state: %w", err)
	}
	if dirtyMigration || currentMigration != latestMigration {
		shutdownTelemetry(log, tel)
		return nil, fmt.Errorf("verify database migration state: current=%d latest=%d dirty=%t", currentMigration, latestMigration, dirtyMigration)
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
		// The connection retry is bounded and jittered: a database that is
		// mid-failover refuses connections for a while, and that pause must
		// not become a crash loop.
		Log:             log,
		ConnectAttempts: cfg.DatabaseConnectAttempts,
		ConnectDelay:    cfg.DatabaseConnectDelay,
	}, configCipher)
	if err != nil {
		shutdownTelemetry(log, tel)
		return nil, err
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
	log.Info("database ready", "backend", store.BackendName(cfg.DatabaseURL))
	if cfg.ReplicaDatabaseURL != "" {
		// The URL itself stays out of the line: it carries credentials. The
		// host is the part an operator checks, and LogAttrs already printed it
		// redacted at startup.
		log.Info("read replica enabled", "routed_reads", "monitors list, alert search, stats, alert counts by day")
	}

	srv := &server{
		cfg:             cfg,
		log:             log,
		store:           st,
		tel:             tel,
		shutdownTimeout: 10 * time.Second,
	}
	// From here on the server owns the store and the tracer, so a failed
	// build releases them rather than leaving a pool and a batcher behind.
	built := false
	defer func() {
		if !built {
			srv.Close()
		}
	}()

	leader, err := deps.newLeader(cfg, log)
	if err != nil {
		return nil, err
	}
	srv.leader = leader

	// Postgres partitions alerts by month. Make sure the months just ahead
	// exist before the poller can write into them, so a row never has to fall
	// back to the default partition under normal operation. A no-op on SQLite.
	if pe, ok := st.(store.PartitionEnsurer); ok {
		if err := pe.EnsureAlertPartitions(ctx, time.Now().UTC(), 3); err != nil {
			return nil, err
		}
		checker, ok := st.(store.PartitionRunwayChecker)
		if !ok {
			return nil, errors.New("partition backend does not support runway checks")
		}
		if err := checker.CheckAlertPartitionRunway(ctx, time.Now().UTC(), 3); err != nil {
			return nil, fmt.Errorf("verify alert partition runway: %w", err)
		}
	}

	// Tenancy. The configured credentials name the workspaces this instance
	// serves, so the table is written from configuration at startup. It is
	// idempotent by design: a restart records the same tenants and never
	// overwrites a name, and 'default' already exists from the migration.
	for _, ws := range cfg.Workspaces() {
		if err := st.EnsureWorkspace(ctx, ws); err != nil {
			return nil, err
		}
	}

	// Scoped API tokens. One manager, shared by the API and the dashboard, so
	// a token minted from either is subject to the same rules about what it
	// may hold. The authenticator gets it too: a bearer string shaped like a
	// token (auth.TokenPrefix) is authenticated through this, while API_TOKEN
	// and WORKSPACE_TOKENS stay the unrestricted operator credentials.
	tokens := auth.NewManager(st, log)
	authn.WithTokens(tokens)

	// Pipeline: event source -> rules -> alerts -> channels. The source is
	// the single seam between the poller and wherever events come from. The
	// backfill subcommand shares this wiring so a replay reads exactly what
	// live monitoring reads.
	src, health, err := deps.buildSource(ctx, log, cfg, st)
	if err != nil {
		return nil, err
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
	factory := notify.DefaultFactory().WithFederationOrigin(cfg.InstanceName)
	// External secrets: when a provider is configured, ${secret:...}
	// references in channel configs resolve when a notifier is built. The
	// stored config keeps the reference.
	if resolver := buildSecretResolver(cfg, log); resolver != nil {
		factory.WithSecrets(resolver)
	}
	validationCtx, cancelValidation := context.WithTimeout(ctx, startupNetworkTimeout)
	channels, err := st.ListChannels(workspace.WithSystem(validationCtx), false)
	if err != nil {
		cancelValidation()
		return nil, fmt.Errorf("validate channel configurations: %w", err)
	}
	var channelErrors []error
	for _, channel := range channels {
		if _, err := validateChannelConfiguration(validationCtx, factory, channel, cfg.SecretsProvider); err != nil {
			channelErrors = append(channelErrors, fmt.Errorf("channel %d (%s): %s", channel.ID, channel.Type, redactPreflightError(err, channelConfigValues(channel.Config)...)))
		}
	}
	cancelValidation()
	if len(channelErrors) > 0 {
		return nil, fmt.Errorf("validate channel configurations: %w", errors.Join(channelErrors...))
	}
	// Channel health: delivery outcomes are folded into each channel so a
	// revoked token surfaces as a broken channel instead of as silence. The
	// threshold is off unless the operator sets it — auto-disabling a channel
	// is a destructive answer to a temporary problem.
	dispatcher := notify.NewDispatcher(st, factory, log).
		WithRateLimits(notify.RateLimits{
			SlackRPS:     cfg.NotifyRateLimitSlackRPS,
			TelegramRPS:  cfg.NotifyRateLimitTelegramRPS,
			PagerDutyRPS: cfg.NotifyRateLimitPagerDutyRPS,
			DefaultRPS:   cfg.NotifyRateLimitDefaultRPS,
		}).
		WithMetrics(m).
		WithDigestQueue(st).
		WithDisableAfterFailures(cfg.ChannelDisableAfterFailures).
		WithFlapping(alerts.FlappingConfig{
			Episodes:   cfg.FlappingEpisodes,
			Window:     cfg.FlappingWindow,
			QuietAfter: cfg.FlappingQuietAfter,
		})
	enricher, err := alerts.NewEnricher(cfg.AlertEnrichmentURL, cfg.AlertEnrichmentTimeout, cfg.AlertEnrichmentCacheTTL)
	if err != nil {
		return nil, err
	}
	// One in-process fan-out carries newly created alerts to the SSE endpoint.
	// The poller publishes into exactly the instance the API serves from, so
	// /alerts/stream needs no database round-trip to show a live alert.
	liveAlerts := broadcast.New(broadcast.DefaultBuffer)
	m.RegisterStreamDropped(liveAlerts.Dropped)
	p := poller.New(src, st, registry, dispatcher, cfg.PollInterval, log).
		WithMetrics(m).
		WithPublisher(liveAlerts).
		WithEnricher(enricher).
		WithReorg(cfg.ReorgTrackingWindow, cfg.ReorgConfirmationDepth)

	// Heartbeat (HEARTBEAT_URL): an external dead-man's switch fed only
	// while ingest is demonstrably healthy. It reads the same position
	// snapshot /readyz uses and shares the readiness lag threshold, so
	// "healthy" means the same thing in both places, and it reuses the
	// store's Ping for the database half of the question. Built here but
	// started inside job, so with several replicas only the leader pings.
	var heartbeat *poller.Heartbeat
	if cfg.HeartbeatURL != "" {
		heartbeat = poller.NewHeartbeat(poller.HeartbeatConfig{
			URL:           cfg.HeartbeatURL,
			Interval:      cfg.HeartbeatInterval,
			FailureSuffix: cfg.HeartbeatFailureSuffix,
			LagThreshold:  cfg.ReadyzLagThreshold,
		}, p, st.Ping, m, log)
		// The URL is a credential; only the shape of the configuration is
		// logged, never anything derived from the URL itself.
		log.Info("external heartbeat enabled",
			"interval", cfg.HeartbeatInterval.String(),
			"failure_pings", cfg.HeartbeatFailureSuffix != "")
	}

	// HTTP: JSON API under /api/v1, dashboard at /.
	apiSrv := api.New(st, registry, factory, health, log).
		WithPoller(p).
		WithNetworks(config.NetworkNames(cfg.Networks)).
		WithLeadership(leader).
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
		return nil, err
	}
	webSrv.WithPoller(p).WithLeadership(leader).WithSilentAfter(cfg.MonitorSilentAfter).WithAuth(authn).
		WithNetworks(config.NetworkNames(cfg.Networks)).WithTokens(tokens)
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

	// GraphQL endpoint at /graphql
	graphqlResolver := graphql.NewResolver(st, log)
	graphqlConfig := graphql.DefaultServerConfig()
	graphqlConfig.EnablePlayground = cfg.GraphQL.EnablePlayground
	graphqlConfig.MaxDepth = cfg.GraphQL.MaxDepth
	graphqlConfig.MaxComplexity = cfg.GraphQL.MaxComplexity
	root.Handle("/graphql", graphql.NewHandler(graphqlResolver, graphqlConfig))
	if cfg.GraphQL.EnablePlayground {
		root.Handle("/graphql/playground", graphql.PlaygroundHandler("/graphql"))
		log.Info("GraphQL playground enabled", "path", "/graphql/playground")
	}

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Retention can tier expired alerts to object storage before deleting
	// them. Off by default: an empty ARCHIVE_URL leaves the pruner behaving
	// exactly as it did before archiving existed. Built here rather than
	// inside the job so a bad ARCHIVE_URL fails startup rather than the first
	// promotion.
	var archiver store.AlertArchiver
	if cfg.ArchiveURL != "" {
		back, err := archive.FromURL(cfg.ArchiveURL)
		if err != nil {
			return nil, err
		}
		pruner, err := archive.NewPruner(back)
		if err != nil {
			return nil, err
		}
		archiver = pruner
		// Never log the URL: an operator may embed an endpoint or token in a
		// query string. The scheme is enough to confirm what was selected.
		log.Info("alert archiving enabled")
	}
	if cfg.AlertRetention <= 0 && archiver != nil {
		// Archiving only happens before a delete, so it is inert without
		// retention. Warn rather than silently doing nothing.
		log.Warn("ARCHIVE_URL is set but ALERT_RETENTION is unset; nothing will be archived or deleted")
	}

	srv.poller = p
	srv.dispatcher = dispatcher
	srv.heartbeat = heartbeat
	srv.archiver = archiver
	srv.httpSrv = httpSrv
	srv.prunerEnabled = cfg.AlertRetention > 0
	srv.playground = cfg.GraphQL.EnablePlayground

	// gRPC serves reads exactly as the HTTP API does, so every instance runs
	// it regardless of leadership.
	if cfg.GRPCAddr != "" {
		srv.grpcSrv = sorogrpc.New(st, authn, log)
	}

	// Bind last. A listener is the first thing a failed startup could leave
	// held, so it is acquired only once every component has been built and
	// closed by Close on any error from here on.
	httpLis, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("http listen %s: %w", cfg.HTTPAddr, err)
	}
	srv.httpListener = httpLis
	if srv.grpcSrv != nil {
		grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
		if err != nil {
			// A gRPC bind failure has always been logged and survived rather
			// than fatal to a deployment whose API is otherwise healthy, and
			// that behaviour is kept: the API is the control plane.
			log.Error("grpc server error", "err", fmt.Errorf("grpc listen %s: %w", cfg.GRPCAddr, err))
			srv.grpcSrv = nil
		} else {
			srv.grpcListener = grpcLis
		}
	}

	built = true
	return srv, nil
}

// run starts the servers and the leader loop, then serves until ctx is
// cancelled or a server fails. Shutdown is ordered: the listeners stop
// accepting, the leader loop is waited for (which stops the poller and
// releases the lease), and only then does run return. Every wait is bounded
// so a wedged component cannot hang a deploy.
func (s *server) run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpErr := make(chan error, 1)
	httpStopped := false
	go func() {
		s.log.Info("http server listening", "addr", s.httpListener.Addr().String(), "rpc_url", s.cfg.RPCURL)
		err := s.httpSrv.Serve(s.httpListener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
			return
		}
		httpErr <- nil
	}()

	var grpcDone chan struct{}
	if s.grpcSrv != nil {
		grpcDone = make(chan struct{})
		go func() {
			defer close(grpcDone)
			if err := s.grpcSrv.ServeListener(runCtx, s.grpcListener); err != nil {
				s.log.Error("grpc server error", "err", err)
			}
		}()
	}

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		// sysCtx, not runCtx: everything this job drives — the poller, the
		// retention pruner, the digest flusher, the escalation stepper — acts
		// across every workspace, and a plain context would be read by the
		// store as a request from the default tenant.
		s.leader.Run(workspace.WithSystem(runCtx), s.job)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		s.log.Info("shutting down")
	case err := <-httpErr:
		// Serve only reports here when it failed, since ErrServerClosed is
		// filtered out above; the goroutine has returned.
		httpStopped = true
		runErr = err
		if err != nil {
			s.log.Error("http server error", "err", err)
		}
	}
	cancel()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancelShutdown()
	if err := s.httpSrv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = err
	}
	// Wait for the election loop to finish before exiting: on the way out it
	// releases the advisory lock, so the surviving instances promote at once
	// instead of waiting for this session to disappear. It also waits for the
	// job it started, which is how the poller, pruner and dispatchers are
	// known to have stopped before the process returns.
	<-leaderDone
	if grpcDone != nil {
		<-grpcDone
	}
	if !httpStopped {
		<-httpErr
	}
	return runErr
}

// job is what the leader runs for as long as it holds the lease. Every loop
// that writes on a schedule lives here rather than in run, because two
// instances doing this work at once would deliver each alert twice. The
// WaitGroup is what lets the lease be handed over safely: the loop does not
// return until the pruner and both dispatcher loops have stopped, so a new
// leader cannot start while this one is still mid-cycle.
func (s *server) job(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(loop func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx)
		}()
	}
	if s.prunerEnabled {
		start(func(ctx context.Context) {
			store.RunAlertPruner(ctx, s.store, s.cfg.AlertRetention, store.DefaultPruneInterval, store.DefaultPruneBatch, s.archiver, s.log)
		})
	}
	// The digest flusher and the escalation stepper are leader-gated for the
	// same reason as the pruner: they send on a timer, and two instances
	// flushing the same pending messages would deliver each one twice.
	start(func(ctx context.Context) {
		s.dispatcher.RunDigestFlusher(ctx, notify.DefaultDigestFlushInterval)
	})
	start(func(ctx context.Context) {
		s.dispatcher.RunEscalations(ctx)
	})
	// The heartbeat runs inside the job rather than beside it: leadership
	// is what gates every send-on-a-timer loop in this process, and only
	// the leader's pings should feed the external service. Run checks
	// immediately on start, so a leadership hand-over cannot open a gap
	// longer than one interval.
	if s.heartbeat != nil {
		start(s.heartbeat.Run)
	}
	s.poller.Run(ctx)
	wg.Wait()
}

// Close releases everything newServer acquired. It is safe on a partially
// built server, which is exactly what an error from newServer relies on.
func (s *server) Close() {
	if s.httpListener != nil {
		_ = s.httpListener.Close()
	}
	if s.grpcListener != nil {
		_ = s.grpcListener.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
	if s.tel != nil {
		shutdownTelemetry(s.log, s.tel)
	}
}

// shutdownTelemetry flushes spans with the same bound an ordinary shutdown
// uses. A collector that has gone away must not turn a startup error into a
// hang.
func shutdownTelemetry(log *slog.Logger, tel *telemetry.Provider) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		log.Warn("flush pending spans on shutdown", "error", err)
	}
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
	case "horizon":
		hc := horizon.NewClient(cfg.HorizonURL, nil)
		log.Info("horizon mode: reading events from Horizon", "url", cfg.HorizonURL)
		// Horizon returns transaction result metadata as XDR, so events must be
		// extracted and decoded through the existing stellar decoder.
		// We use SpecDecoder with a nil spec source (Horizon doesn't provide
		// a spec endpoint), so it falls back to DefaultDecoder for all contracts.
		decoder := stellar.NewSpecDecoder(stellar.DefaultDecoder{}, nil, log)
		return horizon.NewSource(hc, decoder), hc, nil
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

// wiring holds the constructed components so tests can assert that config
// values reach their destinations without starting servers or opening
// databases.
type wiring struct {
	poller          *poller.Poller
	apiSrv          *api.Server
	webSrv          *web.Server
	dispatcher      *notify.Dispatcher
	httpAddr        string
	readyzThreshold uint32
	rateLimit       api.RateLimitConfig
	maxBodyBytes    int64
	silentAfter     time.Duration
	reorgWindow     uint32
	reorgDepth      uint32
}

// runBackfill implements `sorobeacon backfill`: an opt-in historical replay of

type fakeStore struct {
	store.Store
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
	dispatcher := notify.NewDispatcher(st, notify.DefaultFactory().WithFederationOrigin(cfg.InstanceName), log)
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

// warnIfAuthDisabled logs one warning at startup when no credential is
// configured — neither API_TOKEN nor WORKSPACE_TOKENS. Both the API and the
// dashboard stay open, which is how the docker-compose quickstart and every
// existing deployment behave — so this is a warning and not a startup
// failure. The operator should still know: an unauthenticated API can create,
// rewrite and delete monitors and channels from anywhere the port is
// reachable, and with no credential there is nothing to resolve a request's
// workspace from either, so everything is the default tenant's.
func warnIfAuthDisabled(log *slog.Logger, enabled bool) {
	if !enabled {
		log.Warn("API authentication is disabled; set API_TOKEN (or WORKSPACE_TOKENS for one token per workspace, or OIDC_ISSUER for single sign-on) to require a bearer token on /api/v1 and a sign-in on the dashboard")
	}
}

// startupNetworkTimeout bounds the network calls a boot makes: the passphrase
// sweep across every configured endpoint, and OIDC discovery. A set of
// unreachable endpoints delays boot by at most this long rather than once per
// endpoint's own HTTP timeout, and the same is true of a provider that is down.
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

	var mismatches []error
	for _, ep := range client.Endpoints() {
		net, err := ep.Client.GetNetwork(ctx)
		if err != nil {
			log.Warn("could not verify network passphrase", "rpc_url", ep.URL, "error", err)
			continue
		}
		if err := config.VerifyPassphrase(configured, net.Passphrase); err != nil {
			mismatches = append(mismatches, fmt.Errorf("rpc endpoint %s: %w", ep.URL, err))
		}
	}
	return errors.Join(mismatches...)
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
