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
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sorotrail/sorobeacon/internal/api"
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
	levelVar := new(slog.LevelVar)
	levelVar.Set(cfg.LogLevel)
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: levelVar}))
	slog.SetDefault(log)
	log.LogAttrs(context.Background(), slog.LevelInfo, "configuration loaded", cfg.LogAttrs()...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Storage.
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
	log.Info("database ready")

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

		src = poller.NewRPCSource(rpc, stellar.DefaultDecoder{})
		health = rpc
	}
	logStartupHealth(ctx, log, health)

	m := metrics.New()
	registry := rules.NewRegistry()
	factory := notify.DefaultFactory()
	dispatcher := notify.NewDispatcher(st, factory, log).WithMetrics(m)
	p := poller.New(src, st, registry, dispatcher, cfg.PollInterval, log).WithMetrics(m)

	// HTTP: JSON API under /api/v1, dashboard at /.
	apiSrv := api.New(st, registry, factory, health, log).
		WithPoller(p).
		WithReadyzLagThreshold(cfg.ReadyzLagThreshold).
		WithRateLimit(api.RateLimitConfig{
			RPS:            cfg.RateLimitRPS,
			Burst:          cfg.RateLimitBurst,
			TrustForwarded: cfg.RateLimitTrustForwarded,
		}).
		WithMaxBodyBytes(cfg.HTTPMaxBodyBytes)
	webSrv, err := web.New(st, registry, factory, log)
	if err != nil {
		return err
	}
	webSrv.WithPoller(p)
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
	if cfg.AlertRetention > 0 {
		go store.RunAlertPruner(ctx, st, cfg.AlertRetention, store.DefaultPruneInterval, store.DefaultPruneBatch, log)
	}

	live := &liveConfig{cfg: cfg, level: levelVar, poller: p, log: log}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go watchSIGHUP(ctx, hup, live)

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
// liveConfig is the process's current configuration. SIGHUP reloads log
// level and poll interval through it; a mutex keeps Reload from racing
// the next SIGHUP.
type liveConfig struct {
	mu     sync.Mutex
	cfg    config.Config
	level  *slog.LevelVar
	poller *poller.Poller
	log    *slog.Logger
}

func watchSIGHUP(ctx context.Context, hup <-chan os.Signal, live *liveConfig) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			applySIGHUP(live)
		}
	}
}

func applySIGHUP(live *liveConfig) {
	live.mu.Lock()
	defer live.mu.Unlock()
	next, result, err := config.Reload(live.cfg)
	if err != nil {
		live.log.Error("configuration reload rejected", "error", err)
		return
	}
	live.cfg = next
	live.level.Set(next.LogLevel)
	live.poller.SetInterval(next.PollInterval)
	logReload(live.log, result)
}

func logReload(log *slog.Logger, result config.ReloadResult) {
	if len(result.Applied) == 0 && len(result.Skipped) == 0 {
		log.Info("configuration reload", "status", "unchanged")
		return
	}
	for _, c := range result.Applied {
		log.Info("configuration reloaded", "setting", c.Name, "from", c.From, "to", c.To)
	}
	for _, s := range result.Skipped {
		log.Info("configuration reload skipped", "setting", s.Name, "reason", s.Reason, "from", s.From, "to", s.To)
	}
}

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
