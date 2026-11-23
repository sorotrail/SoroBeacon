package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestParsePreflightSelection(t *testing.T) {
	selected, err := parsePreflightSelection("database, source")
	require.NoError(t, err)
	require.True(t, selected["config"])
	require.True(t, selected["database"])
	require.True(t, selected["source"])
	require.False(t, selected["channels"])
	_, err = parsePreflightSelection("smtp")
	require.ErrorContains(t, err, "unknown preflight check")
}

func TestRunPreflightReportsAllDependencyFailures(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	upstreamURL := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())

	report := runPreflight(context.Background(), config.Config{
		DatabaseURL:  filepath.Join(t.TempDir(), "missing.db"),
		SourceMode:   "sorotrail",
		SoroTrailURL: upstreamURL,
	}, preflightOptions{timeout: 2 * time.Second})
	require.Equal(t, "failed", report.Status)
	require.Len(t, report.Checks, 5)
	failed := map[string]bool{}
	for _, check := range report.Checks {
		if check.Status == "fail" {
			failed[check.Name] = true
		}
	}
	require.True(t, failed["database"])
	require.True(t, failed["source"])
	require.True(t, failed["channels"])
	require.True(t, failed["partitions"])
}

func TestRunPreflightCLIConfigFailureIsJSONAndSecretSafe(t *testing.T) {
	t.Setenv("SOURCE_MODE", "config-secret-value")
	var out bytes.Buffer
	err := runPreflightCLI(context.Background(), []string{"--json"}, &out)
	var exitErr *preflightExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, exitPreflightConfig, exitCodeFor(err))
	require.NotContains(t, out.String(), "config-secret-value")
	var report preflightReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	require.Equal(t, "failed", report.Status)
	require.Equal(t, "config", report.Checks[0].Name)
}

func TestRunPreflightCLIDependencyExitCode(t *testing.T) {
	t.Setenv("SOURCE_MODE", "rpc")
	t.Setenv("DATABASE_URL", "sqlite://"+filepath.Join(t.TempDir(), "not-created.db"))
	var out bytes.Buffer
	err := runPreflightCLI(context.Background(), []string{"--json", "--only", "database"}, &out)
	var exitErr *preflightExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, exitPreflightDependency, exitCodeFor(err))
	var report preflightReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	require.Equal(t, "failed", report.Status)
	require.Len(t, report.Checks, 2)
}

func TestRunPreflightCLIHelp(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, runPreflightCLI(context.Background(), []string{"--help"}, &out))
	require.Contains(t, out.String(), "sorobeacon preflight")
}

func TestPreflightExitErrorUnwraps(t *testing.T) {
	base := errors.New("dependency failure")
	err := &preflightExitError{code: exitPreflightDependency, err: base}
	require.ErrorIs(t, err, base)
}

func TestPreflightDoesNotSendUnlessRequested(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	databaseURL := "sqlite://" + filepath.Join(t.TempDir(), "channels.db")
	require.NoError(t, store.Migrate(databaseURL))
	st, err := store.New(context.Background(), databaseURL, store.PoolSettings{}, nil)
	require.NoError(t, err)
	require.NoError(t, st.CreateChannel(context.Background(), &store.Channel{
		Name: "preflight webhook", Type: "webhook", Enabled: true,
		Config: json.RawMessage(`{"url":"` + srv.URL + `","secret":"webhook-secret"}`),
	}))
	st.Close()
	cfg := config.Config{DatabaseURL: databaseURL}

	checks := checkPreflightChannels(context.Background(), cfg, false)
	require.Len(t, checks, 1)
	require.Equal(t, "warning", checks[0].Status)
	require.Zero(t, requests.Load(), "default preflight must never send an alert")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	checks = checkPreflightChannels(ctx, cfg, true)
	require.Len(t, checks, 1)
	require.Equal(t, "pass", checks[0].Status)
	require.EqualValues(t, 1, requests.Load(), "explicit --send-tests should deliver one synthetic alert")
}

func TestValidateChannelConfigurationRejectsUnresolvedReference(t *testing.T) {
	channel := store.Channel{
		Type:   "webhook",
		Config: json.RawMessage(`{"url":"https://hooks.example.test","secret":"${secret:env:HOOK_SECRET}"}`),
	}
	_, err := validateChannelConfiguration(context.Background(), preflightFactory(config.Config{}), channel, "")
	require.ErrorContains(t, err, "SECRETS_PROVIDER is not configured")
}
