package config

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearReloadEnv pins the same baseline the Load tests use: a leaked value
// in any of these variables could otherwise masquerade as a reload change.
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

// loadedReloader is a process that booted on the current environment.
func loadedReloader(t *testing.T) *Reloader {
	t.Helper()
	cfg, err := Load()
	require.NoError(t, err)
	return NewReloader(cfg)
}

// The two reloadable settings apply, old value to new value, and the values
// the caller will hand to the log level and the poller change with them.
func TestReloadAppliesLogLevelAndPollInterval(t *testing.T) {
	clearReloadEnv(t)
	r := loadedReloader(t)
	require.Equal(t, slog.LevelInfo, r.Current().LogLevel)
	require.Equal(t, DefaultPollInterval, r.Current().PollInterval)

	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("POLL_INTERVAL", "15s")

	changed, err := r.Reload()
	require.NoError(t, err)

	assert.Equal(t, []Change{
		{Setting: "log_level", Old: "info", New: "debug"},
		{Setting: "poll_interval", Old: "5s", New: "15s"},
	}, changed.Applied)
	assert.Empty(t, changed.Skipped)
	assert.Equal(t, slog.LevelDebug, r.Current().LogLevel)
	assert.Equal(t, 15*time.Second, r.Current().PollInterval)
}

// A reload that finds nothing new applies nothing and says so by returning
// empty change lists rather than inventing entries.
func TestReloadWithNoChangesReportsNothing(t *testing.T) {
	clearReloadEnv(t)
	r := loadedReloader(t)

	changed, err := r.Reload()
	require.NoError(t, err)
	assert.Empty(t, changed.Applied)
	assert.Empty(t, changed.Skipped)
}

// Table of invalid new configurations. Each one must be rejected wholesale:
// the error names the offending variable and the configuration in effect —
// including a perfectly valid log level arriving alongside a broken one —
// stays exactly as it was. A reload that applied the valid half would leave
// the process running on a configuration nobody chose.
func TestReloadRejectsAnInvalidConfigurationWholesale(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "poll interval below the minimum", key: "POLL_INTERVAL", value: "500ms", want: "POLL_INTERVAL"},
		{name: "poll interval that does not parse", key: "POLL_INTERVAL", value: "soon", want: "POLL_INTERVAL"},
		{name: "log level that does not parse", key: "LOG_LEVEL", value: "verbose", want: "LOG_LEVEL"},
		{name: "invalid listen address", key: "HTTP_ADDR", value: "definitely:not:a:port:8080", want: "HTTP_ADDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearReloadEnv(t)
			r := loadedReloader(t)
			before := r.Current()

			// A valid change rides along with the broken one: it must not
			// be applied either.
			t.Setenv("LOG_LEVEL", "debug")
			t.Setenv(tc.key, tc.value)

			changed, err := r.Reload()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, changed.Applied)
			assert.Empty(t, changed.Skipped)
			assert.Equal(t, before, r.Current())
		})
	}
}

// A changed non-reloadable setting does not fail the reload — the reloadable
// ones still apply — but it is reported as skipped, with the database URL
// redacted the same way LogAttrs redacts it at startup. Silence here would
// leave an operator sending SIGHUP repeatedly and wondering why their edit
// to DATABASE_URL never took.
func TestReloadReportsNonReloadableChangesAsSkipped(t *testing.T) {
	clearReloadEnv(t)
	r := loadedReloader(t)

	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("DATABASE_URL", "postgres://user:secret@db2/sorobeacon")
	t.Setenv("HTTP_ADDR", ":9090")

	changed, err := r.Reload()
	require.NoError(t, err)

	assert.Equal(t, []Change{{Setting: "log_level", Old: "info", New: "warn"}}, changed.Applied)
	assert.Equal(t, []Change{
		// Redacted: scheme, host and database name only. The password the
		// old and new URLs both carry never reaches the change list, and
		// therefore never reaches a log line.
		{Setting: "database_url", Old: "postgres://db1/sorobeacon", New: "postgres://db2/sorobeacon"},
		{Setting: "http_addr", Old: ":8080", New: ":9090"},
	}, changed.Skipped)
	// The settings that could apply stay on their startup values.
	assert.Equal(t, "postgres://user:secret@db1/sorobeacon", r.Current().DatabaseURL)
	assert.Equal(t, ":8080", r.Current().HTTPAddr)
}

// The reload's output is what an operator reads in a log line, so no secret
// may appear in it: a changed API token, a changed encryption key and a
// changed database URL each stay out of both Applied and Skipped.
func TestReloadNeverEchoesSecrets(t *testing.T) {
	clearReloadEnv(t)
	r := loadedReloader(t)

	t.Setenv("API_TOKEN", "super-secret-token-value")
	t.Setenv("CONFIG_ENCRYPTION_KEY", "bm90LWEtcmVhbC1rZXktYnV0LWxvb2tzLWxpa2UtaXQ=")
	t.Setenv("DATABASE_URL", "postgres://other:password@db2/sorobeacon")
	t.Setenv("LOG_LEVEL", "debug")

	changed, err := r.Reload()
	require.NoError(t, err)

	rendered := fmt.Sprintf("%+v %+v", changed.Applied, changed.Skipped)
	assert.NotContains(t, rendered, "super-secret-token-value")
	assert.NotContains(t, rendered, "password")
	assert.NotContains(t, rendered, "bm90LWEtcmVhbC1rZXktYnV0LWxvb2tzLWxpa2UtaXQ=")
}

// Readers race reloads: the API and the poll loop can ask for the current
// configuration at any moment, including mid-reload. `go test -race` is the
// assertion here.
func TestReloadIsSafeAgainstConcurrentCurrent(t *testing.T) {
	clearReloadEnv(t)
	r := loadedReloader(t)
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("POLL_INTERVAL", "7s")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = r.Current()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			_, err := r.Reload()
			require.NoError(t, err)
		}
		close(stop)
	}()
	wg.Wait()

	assert.Equal(t, slog.LevelDebug, r.Current().LogLevel)
	assert.Equal(t, 7*time.Second, r.Current().PollInterval)
}

// The rejection error joins every problem, not just the first, so one SIGHUP
// tells the operator everything wrong with the new configuration.
func TestReloadErrorListsEveryProblem(t *testing.T) {
	clearReloadEnv(t)
	r := loadedReloader(t)

	t.Setenv("POLL_INTERVAL", "soon")
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := r.Reload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POLL_INTERVAL")
	assert.Contains(t, strings.ToLower(err.Error()), "log_level")
}
