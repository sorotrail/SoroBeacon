// Package lease elects a single poller among several SoroBeacon instances.
//
// Every instance serves the API and the dashboard; exactly one holds the
// lease and runs the ingest loop. Leadership is a session-level Postgres
// advisory lock (`pg_try_advisory_lock`), so it needs no table, no migration
// and no coordination process — and because the lock belongs to the session
// that took it, it disappears the moment that session dies. A follower that
// retries the lock therefore promotes itself, without waiting for a timeout
// to expire somewhere.
//
// SQLite has no advisory locks, and a file-backed database cannot be shared
// by several machines anyway, so a SQLite deployment uses SingleNode and is
// always the poller.
package lease

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Key is the advisory lock key SoroBeacon uses for leadership. The only
// requirements on it are that it never changes between releases and that it is
// unlikely to collide with another application's advisory locks in the same
// database; the ASCII spelling ("SOBECON") makes it recognisable in
// `pg_locks`.
const Key int64 = 0x534F4245434F4E

// DefaultInterval is how often a follower retries the lock and a leader
// confirms it still holds it. It bounds failover in both directions: the time
// the cluster can go without a poller after a leader disappears, and the time
// a demoted leader can take to notice. Short enough that an alert is not
// delayed by a restart, long enough that the extra round trips are noise.
const DefaultInterval = 3 * time.Second

// dialTimeout bounds one attempt to open the lease's connection, so a
// black-holed database address cannot stall the campaign loop indefinitely.
const dialTimeout = 10 * time.Second

// releaseTimeout bounds the explicit unlock on shutdown. Unlocking is best
// effort: closing the session releases the lock too, so a slow database only
// costs the fast hand-over, never correctness.
const releaseTimeout = 5 * time.Second

// sessionCloseTimeout bounds closing the lease's connection.
const sessionCloseTimeout = 5 * time.Second

// jobStopTimeout bounds how long the leader waits for its job to return before
// giving the lease up. The poller honours cancellation promptly, so this only
// fires if a job is wedged; waiting forever would instead wedge the process on
// shutdown.
const jobStopTimeout = 10 * time.Second

// Options tunes the election loop.
type Options struct {
	// Interval is how often a follower retries the lock and a leader renews
	// it. Zero means DefaultInterval.
	Interval time.Duration
	// Key is the advisory lock key, which separates one election from another
	// in the same database. Zero means Key, the SoroBeacon default. Override
	// it only for a pool of instances that must not contend with the
	// production ones — the integration tests do, because `make up` leaves a
	// SoroBeacon holding the default key in the very database `make test-db`
	// runs against.
	Key int64
}

func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Key == 0 {
		o.Key = Key
	}
	return o
}

// Session is one dedicated database session. A Postgres advisory lock is
// scoped to the session that took it, so acquire, renew and release all run on
// the same physical connection and the lease never shares it with anything
// else. It exists as an interface so the loop below can be driven by a fake in
// tests.
type Session interface {
	// TryLock takes the advisory lock under key if no other session holds it,
	// reporting whether this session got it. It never blocks.
	TryLock(ctx context.Context, key int64) (bool, error)
	// Renew reports whether the session is still usable and still holds an
	// advisory lock. A false or an error both mean leadership is gone.
	Renew(ctx context.Context) (bool, error)
	// Unlock releases the lock so a follower can take it at once.
	Unlock(ctx context.Context, key int64) error
	// Close ends the session, which releases anything it still holds.
	Close(ctx context.Context) error
}

// Dialer opens a dedicated session. The Postgres dialer implements it, and the
// election loop's tests fake it so they need no database.
type Dialer interface {
	Dial(ctx context.Context) (Session, error)
}

// Status is a race-free view of this instance's leadership, safe to read from
// an HTTP handler while the election loop runs.
type Status struct {
	// Enabled reports whether leader election is active. It is false for a
	// single-node deployment (SQLite), where there is nothing to elect.
	Enabled bool
	// Leader reports whether this instance holds the lease right now.
	Leader bool
	// Since is when the current leadership began; zero when not the leader.
	Since time.Time
}

// Role is the operator-facing label for a status: "leader", "follower" (this
// instance is serving without polling) or "single node" (no election, so this
// instance is the only poller there can be).
func (s Status) Role() string {
	switch {
	case !s.Enabled:
		return "single node"
	case s.Leader:
		return "leader"
	default:
		return "follower"
	}
}

// Lease holds — or competes for — the poller role. It is safe to read Status
// while Run is executing; Run itself must be called once.
type Lease struct {
	dialer Dialer
	opts   Options
	// key is opts.Key after defaulting; the lock this instance competes for.
	key int64
	log *slog.Logger
	// enabled is false for SingleNode: no lock to take, no follower to wait
	// for, so Run simply runs the job.
	enabled bool
	// loggedFailure records that the current run of failures has already been
	// reported, so a database that stays down is logged once per outage rather
	// than once per retry — /api/v1/health already reports it as degraded. Only
	// the Run goroutine touches it.
	loggedFailure bool

	mu     sync.Mutex
	status Status
}

// SingleNode returns a lease for a deployment that cannot have a second
// instance. Nothing is elected, so the instance is the leader from the moment
// it starts; the status still reports leadership, with Enabled false, so the
// probes and the dashboard say so rather than looking like a stuck election.
//
// This is the SQLite case: a file-backed database has no advisory locks, and
// several instances cannot share a file anyway.
func SingleNode(log *slog.Logger) *Lease {
	return &Lease{log: withLogger(log), enabled: false}
}

// newLease builds an election-backed lease. It stays unexported because
// callers pick their dialer by choosing a constructor (NewPostgres for
// Postgres, SingleNode for a backend that has no election at all).
func newLease(d Dialer, opts Options, log *slog.Logger) *Lease {
	opts = opts.withDefaults()
	return &Lease{dialer: d, opts: opts, key: opts.Key, log: withLogger(log), enabled: true}
}

// Run holds the lease until ctx is cancelled, running job in its own goroutine
// for exactly as long as this instance is the leader. job is cancelled and
// waited for before the lease is given up, so a demoted instance stops working
// before another one can start: the overlap would deliver every alert twice.
//
// Run never returns an error. An unreachable database, or a lease a peer
// holds, is a normal follower state rather than a failure of the process — the
// API and dashboard keep serving either way — so both are logged and retried.
func (l *Lease) Run(ctx context.Context, job func(context.Context)) {
	if !l.enabled {
		l.log.Info("leader election is not available for this backend; this instance is the only poller")
		l.set(Status{Leader: true, Since: time.Now()})
		job(ctx)
		l.set(Status{})
		return
	}
	for ctx.Err() == nil {
		l.campaign(ctx, job)
	}
	l.set(Status{Enabled: true})
}

// Status returns the current leadership of this instance.
func (l *Lease) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// set publishes a new status and reports whether it differs from the previous
// one, so callers log transitions instead of every retry.
func (l *Lease) set(next Status) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := l.status.Leader != next.Leader || l.status.Enabled != next.Enabled
	l.status = next
	return changed
}

// campaign makes one attempt at leadership: open a session, take the lock, and
// run the job while the lock holds. It returns once that term of leadership
// ends, having closed the session.
func (l *Lease) campaign(ctx context.Context, job func(context.Context)) {
	sess, err := l.dialer.Dial(ctx)
	if err != nil {
		l.failed("leader election: cannot reach the database; serving without polling", err)
		sleep(ctx, l.opts.Interval)
		return
	}
	if l.loggedFailure {
		l.log.Info("leader election: the database is reachable again")
		l.loggedFailure = false
	}
	defer l.closeSession(ctx, sess)

	got, err := sess.TryLock(ctx, l.key)
	if err != nil {
		l.failed("leader election: could not try the advisory lock; serving without polling", err)
		sleep(ctx, l.opts.Interval)
		return
	}
	if !got {
		// set reports a change, so the line below is logged once per demotion
		// rather than on every retry.
		if l.set(Status{Enabled: true}) {
			l.log.Info("another instance holds the poller lease; serving the API without polling",
				"retry_in", l.opts.Interval)
		}
		sleep(ctx, l.opts.Interval)
		return
	}

	l.hold(ctx, sess, job)
}

// failed reports one attempt that could not reach the database, and keeps this
// instance's status honest about not polling. The line is logged once per
// outage: the retry runs every interval, and repeating the same warning would
// bury the log without telling an operator anything new.
func (l *Lease) failed(msg string, err error) {
	l.set(Status{Enabled: true})
	if l.loggedFailure {
		return
	}
	l.loggedFailure = true
	l.log.Warn(msg, "error", err, "retry_in", l.opts.Interval)
}

// hold runs job for as long as the session keeps holding the lock. It returns
// when the job's context is cancelled (a graceful shutdown) or the lease is
// lost; either way the job has stopped by the time it returns.
func (l *Lease) hold(ctx context.Context, sess Session, job func(context.Context)) {
	l.set(Status{Enabled: true, Leader: true, Since: time.Now()})
	l.log.Info("leadership acquired; the poller is starting", "lock_key", l.key)

	jobCtx, cancelJob := context.WithCancel(ctx)
	jobDone := make(chan struct{})
	go func() {
		defer close(jobDone)
		job(jobCtx)
	}()

	ticker := time.NewTicker(l.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.stop(ctx, sess, cancelJob, jobDone, true)
			return
		case <-ticker.C:
		}

		held, err := sess.Renew(ctx)
		if err == nil && held {
			continue
		}
		if ctx.Err() != nil {
			// The process is shutting down and the renewal raced with it;
			// that is the ordered path, not a lost lease.
			l.stop(ctx, sess, cancelJob, jobDone, true)
			return
		}
		// The lease is gone — the session died, the connection was cut, or
		// the lock was released out from under us. Stop the poller before
		// another instance can win the lock and poll the same events: that
		// overlap is the split-brain case the lease exists to prevent.
		if err != nil {
			l.log.Error("leadership lost: the lease session is gone; the poller is stopping", "error", err)
		} else {
			l.log.Error("leadership lost: the advisory lock is no longer held; the poller is stopping")
		}
		l.stop(ctx, sess, cancelJob, jobDone, false)
		return
	}
}

// stop cancels the job and waits for it to return before the lease is given
// up. Waiting is what makes the hand-over safe: a new leader must not start
// polling while this instance is still in the middle of a cycle. ordered marks
// a graceful shutdown, where the lock is released explicitly so a follower can
// promote itself immediately instead of watching for this session to vanish.
func (l *Lease) stop(ctx context.Context, sess Session, cancelJob context.CancelFunc, jobDone <-chan struct{}, ordered bool) {
	cancelJob()
	select {
	case <-jobDone:
	case <-time.After(jobStopTimeout):
		l.log.Error("the poller did not stop in time; abandoning it",
			"timeout", jobStopTimeout.String())
	}
	if ordered {
		l.release(ctx, sess)
	}
	l.set(Status{Enabled: true})
}

// release gives the advisory lock up explicitly. It runs on a context detached
// from the cancelled one, because on shutdown the whole point is to unlock
// after cancellation.
func (l *Lease) release(ctx context.Context, sess Session) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := sess.Unlock(ctx, l.key); err != nil {
		// Closing the session releases the lock too, so this only costs the
		// fast hand-over.
		l.log.Warn("leader election: could not release the advisory lock on shutdown; the session will release it",
			"error", err)
	}
}

// closeSession ends the session, which releases the advisory lock with it.
func (l *Lease) closeSession(ctx context.Context, sess Session) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCloseTimeout)
	defer cancel()
	_ = sess.Close(ctx)
}

// sleep waits for d or until ctx is done, reporting whether it waited the full
// duration. It returns false when the context ended first, which is how the
// campaign loop learns to stop between attempts.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		// Yield so a zero interval cannot spin the loop.
		d = time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// withLogger falls back to the default logger so a nil log cannot panic.
func withLogger(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.Default()
	}
	return log
}
