package lease

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// testInterval is short enough that a test can watch several renewals and a
// promotion without waiting out the production three seconds; the election
// logic is the same code either way.
const testInterval = 50 * time.Millisecond

// testKey keeps the integration tests off the production lock key, so a
// SoroBeacon left running by `make up` cannot hold the lock these tests need.
const testKey int64 = 0x746573742D6C6561 // "test-lea"

func requirePostgres(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping the Postgres leader-election tests")
	}
	return url
}

// controllableDialer wraps the real Postgres dialer so a test can identify the
// leader's backend — and kill it to sever the connection — and stop an instance
// reconnecting, which turns a promotion into an ordered hand-over instead of a
// race between two hosts.
type controllableDialer struct {
	inner *postgresDialer
	fail  atomic.Bool
	// lastPID is the backend pid of the most recent session, which for a
	// leader is the session holding the lock.
	lastPID atomic.Int32
}

func (d *controllableDialer) Dial(ctx context.Context) (Session, error) {
	if d.fail.Load() {
		return nil, errors.New("simulated database outage")
	}
	sess, err := d.inner.Dial(ctx)
	if err != nil {
		return nil, err
	}
	pg, ok := sess.(*pgSession)
	if !ok {
		return sess, nil
	}
	var pid int32
	if err := pg.conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = sess.Close(ctx)
		return nil, err
	}
	d.lastPID.Store(pid)
	return sess, nil
}

// electionNode is one SoroBeacon instance as far as leader election is
// concerned: its own database connection, its own lease, and the ingest loop
// the lease gates. Two of them against one database behave exactly like two
// replicas — the only state they share is the database — so the hand-over is
// observable without spawning processes.
type electionNode struct {
	name   string
	lease  *Lease
	job    *jobRecorder
	dial   *controllableDialer
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func newNode(t *testing.T, name, url string) *electionNode {
	t.Helper()
	inner, err := newPostgresDialer(url)
	require.NoError(t, err)
	d := &controllableDialer{inner: inner}
	n := &electionNode{
		name:  name,
		lease: newLease(d, Options{Interval: testInterval, Key: testKey}, testLogger()),
		job:   &jobRecorder{},
		dial:  d,
		done:  make(chan struct{}),
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		n.cancel()
		select {
		case <-n.done:
		case <-time.After(10 * time.Second):
			t.Errorf("%s did not shut down", name)
		}
	})
	return n
}

func (n *electionNode) start() {
	go func() {
		defer close(n.done)
		n.lease.Run(n.ctx, n.job.run)
	}()
}

// stop is the graceful-shutdown path: cancel the instance and wait for its
// lease loop to return, which releases the advisory lock on the way out.
func (n *electionNode) stop() {
	n.cancel()
	<-n.done
}

// polling reports whether this instance currently has the ingest loop running.
func (n *electionNode) polling() bool {
	starts, stops, _ := n.job.counts()
	return starts > stops
}

func TestPostgresElectsOnePoller(t *testing.T) {
	url := requirePostgres(t)
	a, b := newNode(t, "a", url), newNode(t, "b", url)
	a.start()
	b.start()

	leader, follower := waitForSingleLeader(t, a, b)
	// A healthy leader must keep the lease: sample both instances for several
	// renewal intervals and fail if the other one ever claims it too.
	assertOnlyOnePolls(t, a, b)

	// A graceful exit releases the lock, so the follower only has to reach its
	// next attempt — bounded by the renewal interval, not by a timeout.
	leader.stop()
	waitFor(t, follower.name+" to promote after the leader exited", func() bool {
		return follower.lease.Status().Leader && follower.polling()
	})
	if leader.polling() {
		t.Fatalf("%s is still polling after shutdown", leader.name)
	}
	if role := follower.lease.Status().Role(); role != "leader" {
		t.Fatalf("promoted instance role = %q, want leader", role)
	}
	assertOnlyOnePolls(t, a, b)
}

// TestPostgresLeaderStopsPollingWhenItsConnectionDies is the split-brain case.
// The advisory lock dies with the session that holds it, so the moment a
// leader's connection is cut another instance can win the lock and start
// polling. If the old leader kept polling, both would ingest the same events
// and deliver every alert twice; it must stop instead.
func TestPostgresLeaderStopsPollingWhenItsConnectionDies(t *testing.T) {
	url := requirePostgres(t)
	a, b := newNode(t, "a", url), newNode(t, "b", url)
	a.start()
	b.start()

	leader, follower := waitForSingleLeader(t, a, b)
	require.NotZero(t, leader.dial.lastPID.Load(), "the leader never reported a backend pid")

	// Sever the connection the way a network drop or a server-side restart
	// does, and keep it severed so the old leader cannot win the lock back
	// before the survivor takes it.
	leader.dial.fail.Store(true)
	terminateBackend(t, url, leader.dial.lastPID.Load())

	waitFor(t, "the severed leader to stop polling", func() bool { return !leader.polling() })
	waitFor(t, follower.name+" to take over", func() bool {
		return follower.lease.Status().Leader && follower.polling()
	})

	// It must stay stopped rather than reconnecting and polling alongside the
	// new leader.
	starts, _, _ := leader.job.counts()
	time.Sleep(6 * testInterval)
	if leader.polling() {
		t.Fatalf("%s resumed polling after losing its database connection", leader.name)
	}
	if now, _, _ := leader.job.counts(); now != starts {
		t.Fatalf("%s started the poller %d more times after losing its connection", leader.name, now-starts)
	}
	assertOnlyOnePolls(t, a, b)
}

// waitForSingleLeader waits until exactly one node holds the lease and is
// polling, and fails the test if both ever do.
func waitForSingleLeader(t *testing.T, a, b *electionNode) (leader, follower *electionNode) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		switch {
		case a.lease.Status().Leader && b.lease.Status().Leader:
			t.Fatalf("both %s and %s hold the lease", a.name, b.name)
		case a.lease.Status().Leader && a.polling():
			return a, b
		case b.lease.Status().Leader && b.polling():
			return b, a
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("neither instance took the poller lease")
	return nil, nil
}

// assertOnlyOnePolls samples both instances for several renewal intervals: two
// leaders, or two running pollers, is the outcome this feature exists to
// prevent, so it is a failure even if it clears up by itself.
func assertOnlyOnePolls(t *testing.T, a, b *electionNode) {
	t.Helper()
	deadline := time.Now().Add(6 * testInterval)
	for time.Now().Before(deadline) {
		if a.lease.Status().Leader && b.lease.Status().Leader {
			t.Fatalf("both %s and %s hold the lease", a.name, b.name)
		}
		if a.polling() && b.polling() {
			t.Fatalf("both %s and %s are polling", a.name, b.name)
		}
		time.Sleep(time.Millisecond)
	}
}

// terminateBackend kills one Postgres backend from a separate connection. The
// backend's exit releases every lock it held, which is what a dropped
// connection does to a leader's advisory lock.
func terminateBackend(t *testing.T, url string, pid int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	var killed bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&killed))
	require.True(t, killed, "pg_terminate_backend(%d) reported no backend killed", pid)
}
