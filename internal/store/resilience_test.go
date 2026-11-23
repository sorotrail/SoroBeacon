package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifySeparatesTransientFromPermanent is the classification the
// whole retry story rests on: a connection that went away is worth another
// attempt, a constraint violation is not, and a deadlock is temporary but
// not an outage.
func TestClassifySeparatesTransientFromPermanent(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		retryable   bool
		unavailable bool
	}{
		{
			name:        "connection failure",
			err:         &pgconn.PgError{Code: "08006"},
			retryable:   true,
			unavailable: true,
		},
		{
			name:        "server closed the connection",
			err:         &pgconn.PgError{Code: "57P01"},
			retryable:   true,
			unavailable: true,
		},
		{
			name:        "too many connections",
			err:         &pgconn.PgError{Code: "53300"},
			retryable:   true,
			unavailable: true,
		},
		{
			name:      "deadlock",
			err:       &pgconn.PgError{Code: "40P01"},
			retryable: true,
		},
		{
			name:      "serialization failure",
			err:       &pgconn.PgError{Code: "40001"},
			retryable: true,
		},
		{
			// The example from the issue: a constraint violation must never
			// be retried, or a permanent failure becomes an infinite loop.
			name: "unique violation",
			err:  &pgconn.PgError{Code: "23505"},
		},
		{
			name: "foreign key violation",
			err:  &pgconn.PgError{Code: "23503"},
		},
		{
			name: "undefined table",
			err:  &pgconn.PgError{Code: "42P01"},
		},
		{
			name: "cancelled context",
			err:  context.Canceled,
		},
		{
			name: "deadline exceeded",
			err:  context.DeadlineExceeded,
		},
		{
			name:        "connection refused",
			err:         &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			retryable:   true,
			unavailable: true,
		},
		{
			name:        "dns failure",
			err:         &net.DNSError{Err: "no such host"},
			retryable:   true,
			unavailable: true,
		},
		{
			name:        "driver level conn closed",
			err:         errors.New("conn closed"),
			retryable:   true,
			unavailable: true,
		},
		{
			name: "plain error",
			err:  errors.New("something else went wrong"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.retryable, IsRetryable(tc.err), "retryable")
			assert.Equal(t, tc.unavailable, IsUnavailable(tc.err), "unavailable")
		})
	}
}

// TestRetryRepeatsTransientFailures proves the retry actually repeats and
// that a success on a later attempt is reported as a success.
func TestRetryRepeatsTransientFailures(t *testing.T) {
	policy := RetryPolicy{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond}
	calls := 0
	err := policy.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return &pgconn.PgError{Code: "08006"}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
}

// TestRetryStopsOnPermanentError is the branch that must never regress: a
// constraint violation is attempted exactly once.
func TestRetryStopsOnPermanentError(t *testing.T) {
	policy := RetryPolicy{Attempts: 5, Base: time.Millisecond, Max: time.Millisecond}
	calls := 0
	err := policy.Do(context.Background(), func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "23505"}
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
}

// TestRetryGivesUpAfterTheAttemptsAreUsedUp proves the retry is bounded: a
// database that stays away produces an error rather than an endless loop.
func TestRetryGivesUpAfterTheAttemptsAreUsedUp(t *testing.T) {
	policy := RetryPolicy{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond}
	calls := 0
	err := policy.Do(context.Background(), func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "08006"}
	})
	require.Error(t, err)
	assert.Equal(t, 3, calls)
}

// TestRetryStopsOnCancelledContext proves a shutdown is not waited out.
func TestRetryStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	policy := RetryPolicy{Attempts: 5, Base: time.Hour, Max: time.Hour}
	calls := 0
	err := policy.Do(ctx, func(context.Context) error {
		calls++
		cancel()
		return &pgconn.PgError{Code: "08006"}
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}

// TestRetryDelayGrowsAndStaysUnderTheCap pins the backoff shape: it
// doubles, it never exceeds the cap, and the jitter keeps it inside the
// upper half of the range.
func TestRetryDelayGrowsAndStaysUnderTheCap(t *testing.T) {
	policy := RetryPolicy{Base: 100 * time.Millisecond, Max: 400 * time.Millisecond}
	assert.Equal(t, 100*time.Millisecond, policy.delay(0))
	assert.Equal(t, 200*time.Millisecond, policy.delay(1))
	assert.Equal(t, 400*time.Millisecond, policy.delay(2))
	assert.Equal(t, 400*time.Millisecond, policy.delay(9))

	jittered := RetryPolicy{Base: 100 * time.Millisecond, Max: time.Second, Jitter: true,
		Rand: func() float64 { return 0 }}
	assert.Equal(t, 50*time.Millisecond, jittered.delay(0))
	full := RetryPolicy{Base: 100 * time.Millisecond, Max: time.Second, Jitter: true,
		Rand: func() float64 { return 1 }}
	assert.Equal(t, 100*time.Millisecond, full.delay(0))
}

// TestDatabaseHealthLogsEachTransitionOnce is the "logged once each, not per
// attempt" requirement: two failures in a row produce one line, and the
// recovery produces one more.
func TestDatabaseHealthLogsEachTransitionOnce(t *testing.T) {
	handler := &recordingHandler{}
	health := NewDatabaseHealth(slog.New(handler))
	require.False(t, health.Degraded())

	health.Observe(&pgconn.PgError{Code: "08006"})
	health.Observe(&pgconn.PgError{Code: "08006"})
	require.True(t, health.Degraded())
	assert.Len(t, handler.records(), 1)

	health.Observe(nil)
	require.False(t, health.Degraded())
	assert.Len(t, handler.records(), 2)

	health.Observe(nil)
	assert.Len(t, handler.records(), 2)
}

// TestDatabaseHealthIgnoresNonOutageFailures proves a deadlock does not
// take the instance out of rotation: it is a temporary problem between two
// sessions, not an unreachable database.
func TestDatabaseHealthIgnoresNonOutageFailures(t *testing.T) {
	handler := &recordingHandler{}
	health := NewDatabaseHealth(slog.New(handler))
	health.Observe(&pgconn.PgError{Code: "40P01"})
	assert.False(t, health.Degraded())
	assert.Empty(t, handler.records())
}

// TestDatabaseHealthRemembersWhenItDegraded proves the transition carries
// the time it happened at, which is what an operator reads in the log.
func TestDatabaseHealthRemembersWhenItDegraded(t *testing.T) {
	health := NewDatabaseHealth(slog.New(&recordingHandler{}))
	health.Observe(&pgconn.PgError{Code: "08006"})
	since, degraded := health.DegradedSince()
	require.True(t, degraded)
	assert.False(t, since.IsZero())
}

// TestStartupRetriesBeforeGivingUp proves the process does not die on the
// first refusal: it tries, waits and tries again, and only then reports
// the failure.
func TestStartupRetriesBeforeGivingUp(t *testing.T) {
	base := 60 * time.Millisecond
	policy := ConnectPolicy(3, base)
	calls := 0
	start := time.Now()
	err := policy.Do(context.Background(), func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "08006"}
	})
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Equal(t, 3, calls)
	// Two waits happened, each at least half the base delay.
	assert.GreaterOrEqual(t, elapsed, base)
}

// TestConnectPolicyDefaults proves the zero value still means something
// sensible: a bounded number of attempts, not an unbounded loop.
func TestConnectPolicyDefaults(t *testing.T) {
	policy := ConnectPolicy(0, 0)
	assert.Equal(t, DefaultConnectAttempts, policy.Attempts)
	assert.Equal(t, DefaultConnectDelay, policy.Base)
}

// TestPostgresSurvivesDatabaseGoingAway is the mid-run connectivity loss
// the issue asks for. The database is taken away behind a proxy the test
// can close, and the store has to notice, say so once, and come back when
// the database returns.
func TestPostgresSurvivesDatabaseGoingAway(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres resilience test")
	}
	require.NoError(t, Migrate(url))

	proxy := newTCPProxy(t, hostPort(t, url))
	proxied := withHost(t, url, proxy.address())

	ctx := context.Background()
	st, err := NewPostgres(ctx, proxied, PoolSettings{})
	require.NoError(t, err)
	t.Cleanup(st.Close)

	// The store works before anything goes wrong.
	require.NoError(t, st.Ping(ctx))
	require.False(t, st.Degraded())

	// The database goes away.
	proxy.stop()

	// The ping fails, and the store says it is degraded rather than
	// pretending the last successful call still counts.
	err = st.Ping(ctx)
	require.Error(t, err)
	assert.True(t, st.Degraded(), "a failed ping must mark the store degraded")

	// A read fails too, and it fails with the error rather than hanging.
	_, err = st.ListMonitors(ctx, true)
	require.Error(t, err)

	// The database comes back, and so does the store.
	proxy.start()
	require.Eventually(t, func() bool {
		return st.Ping(ctx) == nil
	}, 15*time.Second, 100*time.Millisecond, "the store did not recover after the database returned")
	assert.False(t, st.Degraded(), "a successful ping must clear the degraded state")

	// And it still serves reads. The database is shared with the rest of the
	// suite, so this asserts the query succeeds rather than that the table is
	// empty.
	_, err = st.ListMonitors(ctx, true)
	require.NoError(t, err)
}

// TestPostgresStartRefusesFastOnPermanentFailure proves a wrong password is
// not retried: the failure is permanent, so waiting for it to change would
// only delay the operator's fix.
func TestPostgresStartRefusesFastOnPermanentFailure(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres resilience test")
	}
	broken := withPassword(t, url, "definitely-not-the-password")
	start := time.Now()
	_, err := NewPostgres(context.Background(), broken, PoolSettings{
		ConnectAttempts: 5,
		ConnectDelay:    2 * time.Second,
	})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "a permanent failure must not be waited out")
}

// recordingHandler captures log records so a test can count them.
type recordingHandler struct {
	mu       sync.Mutex
	captured []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.captured = append(h.captured, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.captured...)
}

// tcpProxy forwards TCP connections to a fixed address and can be stopped
// and started again on the same port. It is how the test takes the database
// away mid-run without stopping Postgres itself.
type tcpProxy struct {
	mu       sync.Mutex
	listener net.Listener
	addr     string
	target   string
	conns    []net.Conn
}

// newTCPProxy starts a proxy in front of target and returns it.
func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &tcpProxy{listener: ln, addr: ln.Addr().String(), target: target}
	go p.serve(ln)
	t.Cleanup(p.stop)
	return p
}

// address is the host:port a client should connect to.
func (p *tcpProxy) address() string { return p.addr }

// stop closes the listener and every live connection, so the client sees
// the database disappear.
func (p *tcpProxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener != nil {
		p.listener.Close() //nolint:errcheck // the listener is being torn down
		p.listener = nil
	}
	for _, c := range p.conns {
		c.Close() //nolint:errcheck // the connection is being torn down
	}
	p.conns = nil
}

// start listens again on the same port, so the client reconnects to the
// same address.
func (p *tcpProxy) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener != nil {
		return
	}
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return
	}
	p.listener = ln
	go p.serve(ln)
}

func (p *tcpProxy) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		p.track(conn)
		go p.forward(conn)
	}
}

func (p *tcpProxy) forward(conn net.Conn) {
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		conn.Close() //nolint:errcheck // the connection is being torn down
		return
	}
	p.track(upstream)
	defer upstream.Close()
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		io.Copy(upstream, conn) //nolint:errcheck // the copy ends when either side closes
		close(done)
	}()
	io.Copy(conn, upstream) //nolint:errcheck // the copy ends when either side closes
	<-done
}

func (p *tcpProxy) track(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns = append(p.conns, c)
}

// hostPort returns the host:port of a database URL.
func hostPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Host
}

// withHost returns the database URL with its host replaced.
func withHost(t *testing.T, raw, host string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	u.Host = host
	return u.String()
}

// withPassword returns the database URL with a different password.
func withPassword(t *testing.T, raw, password string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String()
}

// compile-time check that the postgres store reports its degradation
// through the optional interface the readiness probe looks for.
var _ DegradationReporter = (*Postgres)(nil)
