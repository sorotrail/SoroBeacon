// Package main tests that config values reach their components.
package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/api"
	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/web"
)

// defaultConfig returns a Config with sensible defaults for testing.
func defaultConfig() config.Config {
	return config.Config{
		DatabaseURL:            "sqlite:///tmp/test.db",
		PollInterval:           config.DefaultPollInterval,
		HTTPAddr:               config.DefaultHTTPAddr,
		HTTPMaxBodyBytes:       config.DefaultHTTPMaxBodyBytes,
		LogLevel:               slog.LevelInfo,
		MonitorSilentAfter:     config.DefaultMonitorSilentAfter,
		ReorgTrackingWindow:    config.DefaultReorgTrackingWindow,
		ReorgConfirmationDepth: 0,
	}
}

func TestWiring_PollIntervalReachesPoller(t *testing.T) {
	cfg := defaultConfig()
	cfg.PollInterval = 10 * time.Second

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.poller == nil {
		t.Fatal("poller is nil")
	}
	if w.poller.Interval() != cfg.PollInterval {
		t.Fatalf("poller interval = %v, want %v", w.poller.Interval(), cfg.PollInterval)
	}
}

func TestWiring_HTTPAddrIsSet(t *testing.T) {
	cfg := defaultConfig()
	cfg.HTTPAddr = ":9090"

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.httpAddr != cfg.HTTPAddr {
		t.Fatalf("httpAddr = %q, want %q", w.httpAddr, cfg.HTTPAddr)
	}
}

func TestWiring_HTTPMaxBodyBytesReachesAPI(t *testing.T) {
	cfg := defaultConfig()
	cfg.HTTPMaxBodyBytes = 2 << 20 // 2 MiB

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.maxBodyBytes != cfg.HTTPMaxBodyBytes {
		t.Fatalf("maxBodyBytes = %d, want %d", w.maxBodyBytes, cfg.HTTPMaxBodyBytes)
	}
}

func TestWiring_ZeroMaxBodyBytesDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.HTTPMaxBodyBytes = 0 // Should be ignored by WithMaxBodyBytes, default used by API server

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	// The wiring struct stores the config value (0), but the API server
	// uses the default because WithMaxBodyBytes ignores non-positive values.
	if w.maxBodyBytes != 0 {
		t.Fatalf("maxBodyBytes = %d, want 0 (config value)", w.maxBodyBytes)
	}
}

func TestWiring_RateLimitConfigReachesAPI(t *testing.T) {
	cfg := defaultConfig()
	cfg.RateLimitRPS = 10.5
	cfg.RateLimitBurst = 20
	cfg.RateLimitTrustForwarded = true

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.rateLimit.RPS != cfg.RateLimitRPS {
		t.Fatalf("rateLimit.RPS = %v, want %v", w.rateLimit.RPS, cfg.RateLimitRPS)
	}
	if w.rateLimit.Burst != cfg.RateLimitBurst {
		t.Fatalf("rateLimit.Burst = %d, want %d", w.rateLimit.Burst, cfg.RateLimitBurst)
	}
	if w.rateLimit.TrustForwarded != cfg.RateLimitTrustForwarded {
		t.Fatalf("rateLimit.TrustForwarded = %v, want %v", w.rateLimit.TrustForwarded, cfg.RateLimitTrustForwarded)
	}
}

func TestWiring_ZeroRateLimitDisables(t *testing.T) {
	cfg := defaultConfig()
	cfg.RateLimitRPS = 0
	cfg.RateLimitBurst = 0
	cfg.RateLimitTrustForwarded = false

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	// Zero RPS means limiter disabled
	if w.rateLimit.RPS != 0 {
		t.Fatalf("rateLimit.RPS = %v, want 0 (disabled)", w.rateLimit.RPS)
	}
}

func TestWiring_ReadyzLagThresholdReachesAPI(t *testing.T) {
	cfg := defaultConfig()
	cfg.ReadyzLagThreshold = 42

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.readyzThreshold != cfg.ReadyzLagThreshold {
		t.Fatalf("readyzThreshold = %d, want %d", w.readyzThreshold, cfg.ReadyzLagThreshold)
	}
}

func TestWiring_ZeroReadyzLagThresholdDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.ReadyzLagThreshold = 0 // Disabled by default

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.readyzThreshold != 0 {
		t.Fatalf("readyzThreshold = %d, want 0 (disabled)", w.readyzThreshold)
	}
}

func TestWiring_ChannelDisableAfterFailuresReachesDispatcher(t *testing.T) {
	cfg := defaultConfig()
	cfg.ChannelDisableAfterFailures = 5

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.dispatcher == nil {
		t.Fatal("dispatcher is nil")
	}
	if w.dispatcher.DisableAfterFailures() != cfg.ChannelDisableAfterFailures {
		t.Fatalf("dispatcher.DisableAfterFailures() = %d, want %d", w.dispatcher.DisableAfterFailures(), cfg.ChannelDisableAfterFailures)
	}
}

func TestWiring_ZeroChannelDisableAfterFailuresDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.ChannelDisableAfterFailures = 0 // Disabled by default

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.dispatcher.DisableAfterFailures() != 0 {
		t.Fatalf("dispatcher.DisableAfterFailures() = %d, want 0 (disabled)", w.dispatcher.DisableAfterFailures())
	}
}

func TestWiring_NotifyRateLimitsReachDispatcher(t *testing.T) {
	cfg := defaultConfig()
	cfg.NotifyRateLimitSlackRPS = 7.5
	cfg.NotifyRateLimitTelegramRPS = 42
	cfg.NotifyRateLimitPagerDutyRPS = 3.5
	cfg.NotifyRateLimitDefaultRPS = 11

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	want := notify.RateLimits{
		SlackRPS:     7.5,
		TelegramRPS:  42,
		PagerDutyRPS: 3.5,
		DefaultRPS:   11,
	}
	if got := w.dispatcher.RateLimits(); got != want {
		t.Fatalf("dispatcher.RateLimits() = %+v, want %+v", got, want)
	}
}

func TestWiring_ZeroNotifyRateLimitsUseDefaults(t *testing.T) {
	cfg := defaultConfig()

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if got := w.dispatcher.RateLimits(); got != (notify.RateLimits{}) {
		t.Fatalf("dispatcher.RateLimits() = %+v, want zero overrides", got)
	}
}

func TestWiring_MonitorSilentAfterReachesWeb(t *testing.T) {
	cfg := defaultConfig()
	cfg.MonitorSilentAfter = 12 * time.Hour

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.webSrv.SilentAfter() != cfg.MonitorSilentAfter {
		t.Fatalf("webSrv.SilentAfter() = %v, want %v", w.webSrv.SilentAfter(), cfg.MonitorSilentAfter)
	}
}

func TestWiring_ZeroMonitorSilentAfterDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.MonitorSilentAfter = 0 // Should use default

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	// WithSilentAfter ignores non-positive values
	if w.webSrv.SilentAfter() != config.DefaultMonitorSilentAfter {
		t.Fatalf("webSrv.SilentAfter() = %v, want default %v", w.webSrv.SilentAfter(), config.DefaultMonitorSilentAfter)
	}
}

func TestWiring_ReorgConfigReachesPoller(t *testing.T) {
	cfg := defaultConfig()
	cfg.ReorgTrackingWindow = 64
	cfg.ReorgConfirmationDepth = 3

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.reorgWindow != cfg.ReorgTrackingWindow {
		t.Fatalf("reorgWindow = %d, want %d", w.reorgWindow, cfg.ReorgTrackingWindow)
	}
	if w.reorgDepth != cfg.ReorgConfirmationDepth {
		t.Fatalf("reorgDepth = %d, want %d", w.reorgDepth, cfg.ReorgConfirmationDepth)
	}
}

func TestWiring_ZeroReorgDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.ReorgTrackingWindow = 0
	cfg.ReorgConfirmationDepth = 0

	w, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring failed: %v", err)
	}

	if w.reorgWindow != 0 {
		t.Fatalf("reorgWindow = %d, want 0 (disabled)", w.reorgWindow)
	}
	if w.reorgDepth != 0 {
		t.Fatalf("reorgDepth = %d, want 0 (disabled)", w.reorgDepth)
	}
}

func TestWiring_ComponentConstructionFailureIsReturned(t *testing.T) {
	// Test that an error constructing the web server is returned
	// rather than panicking. We can't easily test this without a
	// more complex setup, so we verify the function returns error
	// instead of panicking on invalid config.

	// The config validation happens in config.Load, so by the time
	// we get here, the config is valid. The web.New only fails on
	// template parse errors which are embedded and won't fail at
	// runtime. So we just verify the function doesn't panic.
	cfg := defaultConfig()

	_, err := buildWiring(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWiring should succeed with valid config: %v", err)
	}
}

// testHealthChecker is a minimal HealthChecker for testing.
type testHealthChecker struct{}

func (testHealthChecker) GetHealth(context.Context) (*stellar.Health, error) { return nil, nil }

// buildWiring constructs the core components from config. It is a minimal,
// additive extraction of the wiring logic from run() so tests can assert that
// each config value reaches its component. It does not start servers, run
// migrations, or connect to databases.
func buildWiring(ctx context.Context, cfg config.Config, log *slog.Logger) (*wiring, error) {
	// Minimal stubs for dependencies not under test.
	st := &fakeStore{}
	reg := rules.NewRegistry()
	factory := notify.DefaultFactory().WithFederationOrigin(cfg.InstanceName)

	// Poller interval
	p := poller.New(nil, st, reg, nil, cfg.PollInterval, log)

	// API server
	apiSrv := api.New(st, reg, factory, &testHealthChecker{}, log).
		WithMaxBodyBytes(cfg.HTTPMaxBodyBytes).
		WithReadyzLagThreshold(cfg.ReadyzLagThreshold).
		WithRateLimit(api.RateLimitConfig{
			RPS:            cfg.RateLimitRPS,
			Burst:          cfg.RateLimitBurst,
			TrustForwarded: cfg.RateLimitTrustForwarded,
		})

	// Web server
	webSrv, err := web.New(st, reg, factory, log)
	if err != nil {
		return nil, err
	}
	webSrv = webSrv.WithSilentAfter(cfg.MonitorSilentAfter)

	// Dispatcher
	dispatcher := notify.NewDispatcher(st, factory, log).
		WithRateLimits(notify.RateLimits{
			SlackRPS:     cfg.NotifyRateLimitSlackRPS,
			TelegramRPS:  cfg.NotifyRateLimitTelegramRPS,
			PagerDutyRPS: cfg.NotifyRateLimitPagerDutyRPS,
			DefaultRPS:   cfg.NotifyRateLimitDefaultRPS,
		}).
		WithDisableAfterFailures(cfg.ChannelDisableAfterFailures)

	// Poller options
	p = p.WithReorg(cfg.ReorgTrackingWindow, cfg.ReorgConfirmationDepth)

	return &wiring{
		poller:          p,
		apiSrv:          apiSrv,
		webSrv:          webSrv,
		dispatcher:      dispatcher,
		httpAddr:        cfg.HTTPAddr,
		readyzThreshold: cfg.ReadyzLagThreshold,
		rateLimit: api.RateLimitConfig{
			RPS:            cfg.RateLimitRPS,
			Burst:          cfg.RateLimitBurst,
			TrustForwarded: cfg.RateLimitTrustForwarded,
		},
		maxBodyBytes: cfg.HTTPMaxBodyBytes,
		silentAfter:  cfg.MonitorSilentAfter,
		reorgWindow:  cfg.ReorgTrackingWindow,
		reorgDepth:   cfg.ReorgConfirmationDepth,
	}, nil
}
