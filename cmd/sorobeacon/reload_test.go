package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/poller"
)

// clearReloadEnv pins the baseline the reload tests boot on, so a leaked
// variable in the developer's environment cannot masquerade as a reload
// change.
func clearReloadEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "postgres://user:secret@db1/sorobeacon")
	t.Setenv("RPC_URL", "")
	t.Setenv("RPC_URLS", "")
	t.Setenv("NETWORK", "")
	t.Setenv("NETWORK_PASSPHRASE", "")
	t.Setenv("POLL_INTERVAL", "")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("GRPC_ADDR", "")
	t.Setenv("INSTANCE_NAME", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("SOURCE_MODE", "")
	t.Setenv("SOROTRAIL_URL", "")
	t.Setenv("HORIZON_URL", "")
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	t.Setenv("CONFIG_ENCRYPTION_KEY", "")
	t.Setenv("API_TOKEN", "")
	t.Setenv("WORKSPACE_TOKENS", "")
	t.Setenv("OIDC_ISSUER", "")
	t.Setenv("OIDC_CLIENT_ID", "")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	t.Setenv("OIDC_REDIRECT_URL", "")
	t.Setenv("OIDC_SCOPES", "")
	t.Setenv("OIDC_WORKSPACE", "")
	t.Setenv("OIDC_WORKSPACE_CLAIM", "")
	t.Setenv("OIDC_ALLOWED_DOMAINS", "")
	t.Setenv("OIDC_LOGIN_STATE_TTL", "")
}

// reloadFixture is a booted process: the configuration in effect, the level
// var the JSON handler reads, and a poller whose interval the reload writes.
type reloadFixture struct {
	reloader *config.Reloader
	level    *slog.LevelVar
	poller   *poller.Poller
	log      *slog.Logger
	buf      *bytes.Buffer
}

func newReloadFixture(t *testing.T) *reloadFixture {
	t.Helper()
	clearReloadEnv(t)
	cfg, err := config.Load()
	require.NoError(t, err)

	var level slog.LevelVar
	level.Set(cfg.LogLevel)
	buf := &bytes.Buffer{}
	return &reloadFixture{
		reloader: config.NewReloader(cfg),
		level:    &level,
		poller:   poller.New(nil, nil, nil, nil, cfg.PollInterval, slog.New(slog.DiscardHandler)),
		log:      slog.New(slog.NewTextHandler(buf, nil)),
		buf:      buf,
	}
}

// The two reloadable settings reach their consumers: the handler's LevelVar
// and the poller's interval both move, and the log line carries the change
// old value to new value.
func TestApplyReloadUpdatesLogLevelAndPollInterval(t *testing.T) {
	f := newReloadFixture(t)
	require.Equal(t, slog.LevelInfo, f.level.Level())
	require.Equal(t, config.DefaultPollInterval, f.poller.Interval())

	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("POLL_INTERVAL", "30s")

	applyReload(f.reloader, f.level, f.poller, f.log)

	assert.Equal(t, slog.LevelDebug, f.level.Level())
	assert.Equal(t, 30*time.Second, f.poller.Interval())
	out := f.buf.String()
	assert.Contains(t, out, "configuration reloaded")
	assert.Contains(t, out, "setting=log_level old=info new=debug")
	assert.Contains(t, out, "setting=poll_interval old=5s new=30s")
}

// An invalid new configuration is rejected wholesale: the consumers keep
// their values and the rejection is logged with the offending variable, so
// an operator can see why the process ignored the signal.
func TestApplyReloadRejectsInvalidConfigurationWholesale(t *testing.T) {
	f := newReloadFixture(t)

	// A valid change and a broken one together — neither may apply.
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("POLL_INTERVAL", "10ms")

	applyReload(f.reloader, f.level, f.poller, f.log)

	assert.Equal(t, slog.LevelInfo, f.level.Level())
	assert.Equal(t, config.DefaultPollInterval, f.poller.Interval())
	out := f.buf.String()
	assert.Contains(t, out, "configuration reload rejected")
	assert.Contains(t, out, "POLL_INTERVAL")
	assert.NotContains(t, out, "setting=log_level")
}

// A non-reloadable setting that changed is reported as skipped — with the
// database URL redacted — while the reloadable ones still apply. Ignoring it
// silently would leave an operator convinced their SIGHUP worked.
func TestApplyReloadReportsSkippedSettings(t *testing.T) {
	f := newReloadFixture(t)

	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("DATABASE_URL", "postgres://user:secret@db2/sorobeacon")

	applyReload(f.reloader, f.level, f.poller, f.log)

	out := f.buf.String()
	assert.Contains(t, out, "reload skipped a changed setting")
	assert.Contains(t, out, "setting=http_addr old=:8080 new=:9090")
	assert.Contains(t, out, "setting=database_url")
	assert.NotContains(t, out, "secret")
	assert.Equal(t, ":8080", f.reloader.Current().HTTPAddr)
}

// The signal loop reacts to a SIGHUP and applies the reload, and stops when
// the shutdown context is cancelled — the same context SIGINT/SIGTERM drive.
func TestReloadLoopAppliesOnSIGHUPAndStopsWithTheContext(t *testing.T) {
	f := newReloadFixture(t)
	sigs := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reloadLoop(ctx, sigs, f.reloader, f.level, f.poller, f.log)
	}()

	t.Setenv("LOG_LEVEL", "warn")
	sigs <- syscall.SIGHUP

	require.Eventually(t, func() bool {
		return f.level.Level() == slog.LevelWarn
	}, 2*time.Second, 5*time.Millisecond, "reload loop never applied the change")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload loop did not stop after its context was cancelled")
	}
}
