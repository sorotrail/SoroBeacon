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
	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/rules"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Storage.
	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	st, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Info("database ready")

	// Pipeline: RPC client -> decoder -> rules -> alerts -> channels.
	rpc := stellar.NewHTTPClient(cfg.RPCURL, nil)

	// Verify the RPC endpoint really is the configured network before any
	// monitor starts evaluating events. A mainnet endpoint behind a testnet
	// config (or the reverse) silently evaluates every rule against the
	// wrong chain — this fails fast instead.
	if net, err := rpc.GetNetwork(ctx); err != nil {
		log.Warn("could not verify network passphrase", "error", err)
	} else if err := config.VerifyPassphrase(cfg.Network.Passphrase, net.Passphrase); err != nil {
		return err
	}
	log.Info("network verified", "network", cfg.Network.Name, "rpc_url", cfg.RPCURL)

	m := metrics.New()
	registry := rules.NewRegistry()
	factory := notify.DefaultFactory()
	dispatcher := notify.NewDispatcher(st, factory, log).WithMetrics(m)
	p := poller.New(rpc, stellar.DefaultDecoder{}, st, registry, dispatcher, cfg.PollInterval, log).WithMetrics(m)

	// HTTP: JSON API under /api/v1, dashboard at /.
	apiSrv := api.New(st, registry, factory, rpc, log)
	webSrv, err := web.New(st, registry, factory, log)
	if err != nil {
		return err
	}
	root := chi.NewRouter()
	root.Use(middleware.Recoverer, reqid.Middleware, requestLogger(log))
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

// requestLogger logs one line per request via slog, keeping chi's default
// logger (and its non-structured output) out of the picture.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.Debug("http request",
				"method", r.Method, "path", r.URL.Path,
				"status", ww.Status(), "duration", time.Since(start),
				"request_id", reqid.From(r))
		})
	}
}
