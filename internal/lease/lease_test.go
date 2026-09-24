package lease

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

// fastInterval keeps the election loop's real timers short enough for a test
// while still exercising the ticker path.
const fastInterval = 2 * time.Millisecond

// waitFor polls cond until it holds or the deadline passes. The loop under
// test uses real timers, so the assertions have to wait for it rather than
// inspecting a step function directly.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// testLogger discards output; the loop's log lines are not what these tests
// assert on. The level is above every real one so the failure-path tests stay
// quiet.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.Level(100)}))
}

// fakeSession is one scripted advisory-lock session: the test decides whether
// the lock is won and whether renewal keeps succeeding, and can then assert on
// the release and close calls.
type fakeSession struct {
	lock     bool
	lockErr  error
	renew    bool
	renewErr error

	mu      sync.Mutex
	unlocks int
	closes  int
}

func (s *fakeSession) TryLock(context.Context, int64) (bool, error) {
	return s.lock, s.lockErr
}

func (s *fakeSession) Renew(context.Context) (bool, error) {
	return s.renew, s.renewErr
}

func (s *fakeSession) Unlock(context.Context, int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unlocks++
	return nil
}

func (s *fakeSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

func (s *fakeSession) calls() (unlocks, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unlocks, s.closes
}

// dialResult is one scripted Dial outcome.
type dialResult struct {
	sess *fakeSession
	err  error
}

// fakeDialer hands out the scripted results in order and repeats the last one
// forever, so a test can describe "a follower that never wins" or "a database
// that never comes back" without counting retries.
type fakeDialer struct {
	mu      sync.Mutex
	results []dialResult
	dials   int
}

func newFakeDialer(results ...dialResult) *fakeDialer {
	if len(results) == 0 {
		panic("fakeDialer needs at least one result")
	}
	return &fakeDialer{results: results}
}

func (d *fakeDialer) Dial(context.Context) (Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	r := d.results[min(d.dials, len(d.results))-1]
	if r.err != nil {
		return nil, r.err
	}
	return r.sess, nil
}

func (d *fakeDialer) attempts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// jobRecorder stands in for the poller: it runs until its context is
// cancelled, counting every term it was started for.
type jobRecorder struct {
	mu      sync.Mutex
	starts  int
	stops   int
	running int
}

func (j *jobRecorder) run(ctx context.Context) {
	j.mu.Lock()
	j.starts++
	j.running++
	j.mu.Unlock()
	<-ctx.Done()
	j.mu.Lock()
	j.stops++
	j.running--
	j.mu.Unlock()
}

func (j *jobRecorder) counts() (starts, stops, running int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.starts, j.stops, j.running
}

// runLease starts the election loop and returns a cancel function plus a
// channel closed when Run has returned, so a test can assert on the shutdown
// path as well as the steady state.
func runLease(t *testing.T, l *Lease, job func(context.Context)) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx, job)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	})
	return cancel, done
}

// waitClosed waits for Run to return, failing rather than hanging the test when
// it does not.
func waitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestLeaseRunsTheJobWhileHoldingTheLock(t *testing.T) {
	sess := &fakeSession{lock: true, renew: true}
	l := newLease(newFakeDialer(dialResult{sess: sess}), Options{Interval: fastInterval}, testLogger())
	job := &jobRecorder{}
	cancel, done := runLease(t, l, job.run)

	waitFor(t, "the job to start", func() bool {
		starts, _, _ := job.counts()
		return starts == 1
	})

	if st := l.Status(); !st.Leader || !st.Enabled || st.Since.IsZero() {
		t.Fatalf("status while leader = %+v, want leader with a Since timestamp", st)
	}
	if role := l.Status().Role(); role != "leader" {
		t.Fatalf("role while leader = %q, want leader", role)
	}

	// A graceful stop must release the lock so a follower takes over at once.
	cancel()
	waitClosed(t, done)
	if starts, stops, running := job.counts(); starts != 1 || stops != 1 || running != 0 {
		t.Fatalf("job counts = starts %d stops %d running %d, want 1/1/0", starts, stops, running)
	}
	if unlocks, closes := sess.calls(); unlocks != 1 || closes != 1 {
		t.Fatalf("session calls = %d unlocks, %d closes; want one of each", unlocks, closes)
	}
	if st := l.Status(); st.Leader {
		t.Fatalf("status after shutdown = %+v, want not leader", st)
	}
}

// TestLostSessionStopsTheJob is the split-brain case: a leader whose database
// connection dies must stop polling, because the moment that connection dies
// the advisory lock is free and a follower can take over. Continuing to poll
// would deliver every alert twice.
func TestLostSessionStopsTheJob(t *testing.T) {
	sess := &fakeSession{lock: true, renewErr: errors.New("connection reset by peer")}
	dialer := newFakeDialer(
		dialResult{sess: sess},
		// After the loss the database stays unreachable, so the test proves
		// the loop stopped polling rather than merely re-dialling.
		dialResult{err: errors.New("dial tcp: connection refused")},
	)
	l := newLease(dialer, Options{Interval: fastInterval}, testLogger())
	job := &jobRecorder{}
	cancel, _ := runLease(t, l, job.run)

	waitFor(t, "the job to start", func() bool {
		starts, _, _ := job.counts()
		return starts == 1
	})
	waitFor(t, "the job to stop after the session died", func() bool {
		_, stops, _ := job.counts()
		return stops == 1
	})
	if st := l.Status(); st.Leader {
		t.Fatalf("status after losing the session = %+v, want not leader", st)
	}

	// The lock died with the session, so there is nothing to unlock.
	if unlocks, _ := sess.calls(); unlocks != 0 {
		t.Fatalf("unlocked %d times on a dead session, want 0", unlocks)
	}

	// Nothing may start again while the database is unreachable.
	time.Sleep(10 * fastInterval)
	cancel()
	if starts, _, _ := job.counts(); starts != 1 {
		t.Fatalf("job started %d times after the lease was lost, want 1", starts)
	}
}

// TestReleasedLockStopsTheJob covers the other way leadership ends: the session
// is alive but the advisory lock is gone, so another instance can be polling.
func TestReleasedLockStopsTheJob(t *testing.T) {
	sess := &fakeSession{lock: true, renew: false}
	dialer := newFakeDialer(
		dialResult{sess: sess},
		// The lock the session lost is still held by whoever took it, so the
		// next attempt cannot win it back.
		dialResult{sess: &fakeSession{lock: false}},
	)
	l := newLease(dialer, Options{Interval: fastInterval}, testLogger())
	job := &jobRecorder{}
	cancel, _ := runLease(t, l, job.run)

	waitFor(t, "the job to stop once the lock is gone", func() bool {
		_, stops, _ := job.counts()
		return stops == 1
	})
	waitFor(t, "the next attempt to find the lock taken", func() bool {
		return dialer.attempts() > 1 && !l.Status().Leader
	})
	time.Sleep(10 * fastInterval)
	cancel()
	if starts, _, _ := job.counts(); starts != 1 {
		t.Fatalf("job started %d times after losing the lock, want 1", starts)
	}
}

func TestFollowerServesWithoutPolling(t *testing.T) {
	sess := &fakeSession{lock: false}
	dialer := newFakeDialer(dialResult{sess: sess})
	l := newLease(dialer, Options{Interval: fastInterval}, testLogger())
	job := &jobRecorder{}
	cancel, _ := runLease(t, l, job.run)

	waitFor(t, "the follower to retry the lock", func() bool { return dialer.attempts() > 1 })
	if starts, _, _ := job.counts(); starts != 0 {
		t.Fatalf("job started %d times as a follower, want 0", starts)
	}
	st := l.Status()
	if st.Leader || !st.Enabled {
		t.Fatalf("follower status = %+v, want Enabled true and Leader false", st)
	}
	if role := st.Role(); role != "follower" {
		t.Fatalf("follower role = %q, want follower", role)
	}
	if unlocks, _ := sess.calls(); unlocks != 0 {
		t.Fatalf("a follower unlocked a lock it never held (%d times)", unlocks)
	}
	cancel()
}

func TestFollowerPromotesWhenTheLockIsFree(t *testing.T) {
	follower := &fakeSession{lock: false}
	winner := &fakeSession{lock: true, renew: true}
	dialer := newFakeDialer(dialResult{sess: follower}, dialResult{sess: winner})
	l := newLease(dialer, Options{Interval: fastInterval}, testLogger())
	job := &jobRecorder{}
	cancel, _ := runLease(t, l, job.run)

	waitFor(t, "promotion to leader", func() bool { return l.Status().Leader })
	waitFor(t, "the poller to start after promotion", func() bool {
		starts, _, _ := job.counts()
		return starts == 1
	})
	cancel()
}

func TestUnreachableDatabaseRetriesWithoutPolling(t *testing.T) {
	dialer := newFakeDialer(dialResult{err: errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")})
	l := newLease(dialer, Options{Interval: fastInterval}, testLogger())
	job := &jobRecorder{}
	cancel, _ := runLease(t, l, job.run)

	waitFor(t, "the election loop to retry connecting", func() bool { return dialer.attempts() >= 3 })
	if starts, _, _ := job.counts(); starts != 0 {
		t.Fatalf("job started %d times with no database, want 0", starts)
	}
	if st := l.Status(); st.Leader {
		t.Fatalf("status with no database = %+v, want not leader", st)
	}
	cancel()
}

func TestSingleNodeRunsTheJobWithoutElection(t *testing.T) {
	l := SingleNode(testLogger())
	job := &jobRecorder{}
	cancel, done := runLease(t, l, job.run)

	waitFor(t, "the job to start", func() bool {
		starts, _, _ := job.counts()
		return starts == 1
	})
	st := l.Status()
	if !st.Leader || st.Enabled {
		t.Fatalf("single-node status = %+v, want Leader true and Enabled false", st)
	}
	if role := st.Role(); role != "single node" {
		t.Fatalf("single-node role = %q, want \"single node\"", role)
	}
	cancel()
	waitClosed(t, done)
	if _, stops, _ := job.counts(); stops != 1 {
		t.Fatalf("job stopped %d times, want 1", stops)
	}
}

func TestStatusRole(t *testing.T) {
	tests := []struct {
		name string
		st   Status
		want string
	}{
		{"leader", Status{Enabled: true, Leader: true}, "leader"},
		{"follower", Status{Enabled: true}, "follower"},
		{"single node", Status{Leader: true}, "single node"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.st.Role(); got != tt.want {
				t.Fatalf("Role() = %q, want %q", got, tt.want)
			}
		})
	}
}
