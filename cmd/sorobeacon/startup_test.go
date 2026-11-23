// Package main tests the startup wiring: what a configuration constructs,
// what a cancelled context stops, and what a failed startup leaves behind.
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/sorotrail/sorobeacon/internal/api"
	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/lease"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// startupTestConfig is a configuration the real wiring can build without a
// database service or a node behind it: SQLite in a temporary directory, both
// listeners on port zero, and an hour between polls so the timing loop does
// not run during a test.
func startupTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := defaultConfig()
	cfg.DatabaseURL = "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.GRPCAddr = ""
	cfg.PollInterval = time.Hour
	cfg.AlertRetention = 0
	cfg.ArchiveURL = ""
	return cfg
}

// stubSource is never polled: with an hour-long interval the tests stop the
// poller long before its first cycle. It exists so the poller has a real
// source rather than a nil one.
type stubSource struct{}

func (stubSource) LatestLedger(context.Context) (uint32, error) { return 0, nil }

func (stubSource) FetchEvents(context.Context, uint32, []poller.Watch, string, int) (poller.FetchPage, error) {
	return poller.FetchPage{}, nil
}

type staticHealth struct{}

func (staticHealth) GetHealth(context.Context) (*stellar.Health, error) {
	return &stellar.Health{Status: "healthy"}, nil
}

// fakeLeader stands in for lease.Lease. It records that it was entered and
// only closes released after the job has returned, which is the contract the
// real lease implements before it gives the advisory lock up.
type fakeLeader struct {
	mu       sync.Mutex
	runs     int
	status   lease.Status
	entered  chan struct{}
	released chan struct{}
}

func newFakeLeader() *fakeLeader {
	return &fakeLeader{entered: make(chan struct{}), released: make(chan struct{})}
}

func (f *fakeLeader) Run(ctx context.Context, job func(context.Context)) {
	f.mu.Lock()
	f.runs++
	f.status = lease.Status{Enabled: true, Leader: true, Since: time.Now()}
	f.mu.Unlock()

	close(f.entered)
	job(ctx)

	f.mu.Lock()
	f.status = lease.Status{Enabled: true}
	f.mu.Unlock()
	close(f.released)
}

func (f *fakeLeader) Status() lease.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeLeader) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

// startupDeps returns the two seams a startup test replaces. The source
// builder also asserts that the store is already usable when it is called,
// which pins the ordering the issue calls out: migrations have run before any
// component that queries the database is wired.
func startupDepsForTest(t *testing.T) (startupDeps, *fakeLeader) {
	t.Helper()
	leader := newFakeLeader()
	deps := startupDeps{
		buildSource: func(ctx context.Context, _ *slog.Logger, _ config.Config, st store.Store) (poller.EventSource, api.HealthChecker, error) {
			if _, err := st.ListMonitors(workspace.WithSystem(ctx), false); err != nil {
				t.Errorf("store is not queryable when the source is wired: %v", err)
			}
			return stubSource{}, staticHealth{}, nil
		},
		newLeader: func(config.Config, *slog.Logger) (leaderLease, error) { return leader, nil },
	}
	return deps, leader
}

func startupLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestStartup_ConfigChoosesComponents asserts the real wiring, not a test
// double: each optional subsystem is constructed exactly when its
// configuration enables it, and the leader loop has not started just because
// the components exist.
func TestStartup_ConfigChoosesComponents(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		cfg := startupTestConfig(t)
		cfg.GRPCAddr = "127.0.0.1:0"
		cfg.AlertRetention = 24 * time.Hour
		cfg.ArchiveURL = filepath.Join(t.TempDir(), "archive")
		cfg.GraphQL.EnablePlayground = true
		cfg.PollInterval = 7 * time.Second
		cfg.ChannelDisableAfterFailures = 3
		deps, leader := startupDepsForTest(t)

		srv, err := newServer(context.Background(), cfg, startupLogger(), deps)
		require.NoError(t, err)
		defer srv.Close()

		assert.NotNil(t, srv.grpcSrv, "GRPC_ADDR set must build the gRPC server")
		assert.NotNil(t, srv.grpcListener, "the gRPC listener is bound before run")
		assert.NotNil(t, srv.httpListener, "the HTTP listener is bound before run")
		assert.Equal(t, "127.0.0.1:0", srv.httpSrv.Addr)
		assert.True(t, srv.prunerEnabled, "ALERT_RETENTION must enable the pruner")
		assert.NotNil(t, srv.archiver, "ARCHIVE_URL must build the archiver")
		assert.True(t, srv.playground, "GraphQL playground follows config")
		assert.Equal(t, 7*time.Second, srv.poller.Interval())
		assert.Equal(t, 3, srv.dispatcher.DisableAfterFailures())
		assert.Zero(t, leader.runCount(), "building the server must not start the election loop")
	})

	t.Run("disabled", func(t *testing.T) {
		cfg := startupTestConfig(t)
		cfg.GRPCAddr = ""
		cfg.AlertRetention = 0
		cfg.ArchiveURL = ""
		cfg.GraphQL.EnablePlayground = false
		deps, _ := startupDepsForTest(t)

		srv, err := newServer(context.Background(), cfg, startupLogger(), deps)
		require.NoError(t, err)
		defer srv.Close()

		assert.Nil(t, srv.grpcSrv, "an empty GRPC_ADDR must leave gRPC disabled")
		assert.Nil(t, srv.grpcListener)
		assert.False(t, srv.prunerEnabled, "no ALERT_RETENTION means no pruner loop")
		assert.Nil(t, srv.archiver, "no ARCHIVE_URL means no archiver")
		assert.False(t, srv.playground)
	})
}

// TestStartup_ShutdownStopsEverythingAndReleasesTheLease is the end-to-end
// shutdown assertion: both servers bind, a cancelled context stops them, the
// job's loops have returned by the time the leader releases the lease, and no
// goroutine survives the call.
func TestStartup_ShutdownStopsEverythingAndReleasesTheLease(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg := startupTestConfig(t)
	cfg.GRPCAddr = "127.0.0.1:0"
	cfg.AlertRetention = time.Hour // exercise the pruner loop, not just the poller
	deps, leader := startupDepsForTest(t)

	srv, err := newServer(context.Background(), cfg, startupLogger(), deps)
	require.NoError(t, err)
	defer srv.Close()

	httpAddr := srv.httpListener.Addr().String()
	grpcAddr := srv.grpcListener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.run(ctx) }()

	// Both listeners were bound by newServer, but only run serves on them.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + httpAddr + "/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode < 500
	}, 5*time.Second, 25*time.Millisecond, "HTTP server did not serve")

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", grpcAddr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 25*time.Millisecond, "gRPC server did not bind")

	select {
	case <-leader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader loop never started")
	}

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within the shutdown timeout")
	}

	select {
	case <-leader.released:
	default:
		t.Fatal("run returned before the leader loop released the lease")
	}
	assert.Equal(t, 1, leader.runCount())

	// Both servers are closed by the time run returns: a new connection is
	// refused, not merely unanswered.
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	_, err = client.Get("http://" + httpAddr + "/")
	assert.Error(t, err, "the HTTP listener must be closed after shutdown")

	conn, err := net.DialTimeout("tcp", grpcAddr, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
	}
	assert.Error(t, err, "the gRPC listener must be closed after shutdown")
}

// TestStartup_FailureLeavesNothingRunning pins that each startup failure is
// returned rather than half-applied, and that the cleanup path leaves no
// listener, store or goroutine behind.
func TestStartup_FailureLeavesNothingRunning(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cases := []struct {
		name    string
		prepare func(t *testing.T, cfg *config.Config)
	}{
		{
			name: "unwritable database",
			prepare: func(t *testing.T, cfg *config.Config) {
				// A directory cannot be opened as a SQLite database, which is
				// the same class of failure as an unreachable database: the
				// migration fails before any component is wired.
				cfg.DatabaseURL = "sqlite://" + t.TempDir()
			},
		},
		{
			name: "unknown backend",
			prepare: func(t *testing.T, cfg *config.Config) {
				cfg.DatabaseURL = "bogus://not-a-backend"
			},
		},
		{
			name: "unusable listen address",
			prepare: func(t *testing.T, cfg *config.Config) {
				cfg.HTTPAddr = "not a listen address"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := startupTestConfig(t)
			tc.prepare(t, &cfg)
			deps, leader := startupDepsForTest(t)

			srv, err := newServer(context.Background(), cfg, startupLogger(), deps)
			require.Error(t, err)
			assert.Nil(t, srv)
			assert.Zero(t, leader.runCount(), "a failed startup must not start the election loop")
		})
	}
}

// TestStartup_InvalidConfigurationFailsBeforeAnythingIsBuilt covers the first
// step of run: configuration is validated before a store, a listener or a
// goroutine exists, so an operator sees the bad value rather than a partial
// process.
func TestStartup_InvalidConfigurationFailsBeforeAnythingIsBuilt(t *testing.T) {
	// config.Load insists on a database URL before it reads anything else, so
	// a valid one is set to reach the deliberately-invalid value under test.
	t.Setenv("DATABASE_URL", "sqlite:///tmp/unused.db")
	t.Setenv("SOURCE_MODE", "not-a-mode")
	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SOURCE_MODE")
}
