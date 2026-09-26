// Command sorobeacon runs the SoroBeacon monitoring service: the event
// poller, the JSON API and the dashboard, all in one process. Given any
// argument it acts as a CLI for a running instance instead — see cli.go.
package main

import (
	"context"
	"errors"
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
	if len(args) > 0 {
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
	//
	// Each credential also carries the workspace it acts on: API_TOKEN
	// entries act on the default workspace and WORKSPACE_TOKENS entries on
	// their own, which is how one instance serves several teams without any
	// of them being able to ask for someone else's data.
	bindings, err := cfg.AuthBindings()
	if err != nil {
		return err
	}
	authn := auth.NewBound(bindings, auth.DefaultSessionTTL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The work this process does on its own behalf — ingesting ledgers,
	// delivering alerts, pruning history — belongs to no single workspace: it
	// acts across all of them. Marking the background context once here is
	// what keeps a store method from silently reading it as a request from the
	// default tenant instead.
	sysCtx := workspace.WithSystem(ctx)

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
			return err
		}
		authn.WithOIDC(provider)
		// The issuer URL is configuration, not a secret; the client secret is
		// not named here and never is.
		log.Info("single sign-on enabled", "issuer", cfg.OIDC.Issuer,
			"workspace", cfg.OIDC.Workspace, "allowed_domains", len(cfg.OIDC.AllowedDomains))
	}
	warnIfAuthDisabled(log, authn.Enabled())

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

	// Tenancy. The configured credentials name the workspaces this instance
	// serves, so the table is written from configuration at startup. It is
	// idempotent by design: a restart records the same tenants and never
	// overwrites a name, and 'default' already exists from the migration.
	for _, ws := range cfg.Workspaces() {
		if err := st.EnsureWorkspace(ctx, ws); err != nil {
			return err
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
	// the single seam between the poller and wherever events come from — and
	// there is one of each per configured network. Two chains are two
	// independent pipelines that happen to share a database: each gets its own
	// RPC client (so failover never crosses chains), its own spec decoder (a
	// contract's spec is chain-specific), its own cursor and its own backoff.
	var srcs []poller.EventSource
	var health api.HealthChecker
	switch cfg.SourceMode {
	case "sorotrail":
		stc := sorotrail.NewClient(cfg.SoroTrailURL, nil)
		srcs = []poller.EventSource{sorotrail.NewSource(stc)}
		health = stc
		log.Info("upstream mode: reading events from SoroTrail", "url", cfg.SoroTrailURL)
	default: // "rpc"
		for _, net := range cfg.Networks {
			// Several endpoints behind one Client: calls try them in the order
			// RPC_URLS lists them and fail over when one rate-limits or goes
			// down. The poller, the spec source and the readiness probe all
			// keep talking to a single stellar.Client, so nothing downstream
			// knows the difference.
			rpc := stellar.NewFailoverClient(net.RPCURLs, nil, log)

			// Verify every RPC endpoint really is the network it is configured
			// for before any monitor starts evaluating events. A mainnet
			// endpoint behind a testnet config (or the reverse) silently
			// evaluates every rule against the wrong chain, and because
			// failover picks a node per call, one mixed endpoint would corrupt
			// the alert stream intermittently — the hardest kind of bug to
			// notice. With several chains configured this check matters more,
			// not less: swapping two chains' endpoints is exactly the typo a
			// NETWORKS rollout makes.
			if err := verifyNetworkEndpoints(ctx, log, rpc, net.Passphrase); err != nil {
				return err
			}
			log.Info("network verified",
				"network", net.Name,
				"rpc_url", net.RPCURL,
				"rpc_endpoint_count", len(net.RPCURLs))

			// Contract specs are fetched lazily per contract and cached, so
			// events from a contract with a spec arrive with named fields while
			// every other contract decodes exactly as before.
			decoder := stellar.NewSpecDecoder(stellar.DefaultDecoder{}, stellar.NewRPCSpecSource(rpc), log)
			srcs = append(srcs, poller.NewRPCSource(rpc, decoder))
			// The probes' single `rpc` dependency stays the primary chain's,
			// so /health and /readyz mean what they meant before: the endpoint
			// the instance's own traffic depends on. The per-network detail
			// they add comes from the supervisor below.
			if health == nil {
				health = rpc
			}
		}
	}
	logStartupHealth(ctx, log, health)

	// The multi-network upgrade. Every row written before this feature has no
	// network label, and the migration deliberately leaves them that way
	// rather than guessing. Label them with the primary network now: that is
	// the chain they were actually polled from, because until this build the
	// primary was the only one. Without this, an upgrading deployment's
	// monitors would belong to no chain and no poller would read them — every
	// existing monitor would silently stop alerting.
	labelled, err := st.AssignLegacyNetwork(ctx, cfg.Networks[0].Name)
	if err != nil {
		return err
	}
	if labelled > 0 {
		log.Info("labelled pre-multi-network rows",
			"network", cfg.Networks[0].Name, "rows", labelled)
	}

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
	dispatcher := notify.NewDispatcher(st, factory, log).WithMetrics(m)
	// One poller per chain, each scoped to its own network: it watches only
	// that network's monitors, advances only that network's checkpoint and
	// retracts only that network's alerts on a reorg. NETWORKS unset means one
	// entry, so this is the shape every deployment has — a single-network
	// instance is the one-network case of the multi-network design, not a
	// separate code path that can drift from it.
	units := make([]poller.Unit, 0, len(cfg.Networks))
	for i, net := range cfg.Networks {
		units = append(units, poller.Unit{
			Network: net.Name,
			Poller: poller.New(srcs[i], st, registry, dispatcher, cfg.PollInterval, log).
				WithMetrics(m).
				WithReorg(cfg.ReorgTrackingWindow, cfg.ReorgConfirmationDepth).
				WithNetwork(net.Name),
		})
	}
	p := poller.NewSupervisor(log, units...)

	// HTTP: JSON API under /api/v1, dashboard at /.
	apiSrv := api.New(st, registry, factory, health, log).
		WithPoller(p).
		WithNetworks(config.NetworkNames(cfg.Networks)).
		WithReadyzLagThreshold(cfg.ReadyzLagThreshold).
		WithRateLimit(api.RateLimitConfig{
			RPS:            cfg.RateLimitRPS,
			Burst:          cfg.RateLimitBurst,
			TrustForwarded: cfg.RateLimitTrustForwarded,
		}).
		WithMaxBodyBytes(cfg.HTTPMaxBodyBytes).
		WithAuth(authn).
		WithTokens(tokens)
	webSrv, err := web.New(st, registry, factory, log)
	if err != nil {
		return err
	}
	webSrv.WithPoller(p).WithSilentAfter(cfg.MonitorSilentAfter).WithAuth(authn).
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
	go p.Run(sysCtx)
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
		go store.RunAlertPruner(sysCtx, st, cfg.AlertRetention, store.DefaultPruneInterval, store.DefaultPruneBatch, archiver, log)
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
