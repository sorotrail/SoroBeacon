package store

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Defaults for the connection retry policy. A managed database that is
// being restarted or a failover in progress routinely refuses connections
// for tens of seconds, and a process that gives up on the first refusal
// turns that pause into an outage.
const (
	// DefaultConnectAttempts is how many times the store tries to reach
	// the database at startup, the first attempt included.
	DefaultConnectAttempts = 5
	// DefaultConnectDelay is the base delay between those attempts. It
	// doubles after every failure, up to MaxConnectDelay.
	DefaultConnectDelay = 2 * time.Second
	// MaxConnectDelay caps one delay between connection attempts.
	MaxConnectDelay = 30 * time.Second
)

// Defaults for the read retry policy. They are short on purpose: a read
// that fails because a connection blinked should be repeated within the
// same poll cycle, not after the next one.
const (
	// DefaultReadAttempts is how many times a read is attempted.
	DefaultReadAttempts = 3
	// DefaultReadDelay is the base delay between read attempts.
	DefaultReadDelay = 200 * time.Millisecond
	// MaxReadDelay caps one delay between read attempts.
	MaxReadDelay = 2 * time.Second
)

// failureKind is what a database error means for the caller.
type failureKind int

const (
	// failurePermanent is an error that will not go away: a constraint
	// violation, a syntax error, a missing row. Repeating the statement
	// changes nothing.
	failurePermanent failureKind = iota
	// failureTransient is an error that may go away on its own: a
	// serialization failure, a deadlock, a server that is still starting.
	failureTransient
	// failureUnavailable is an error that says the database could not be
	// reached at all.
	failureUnavailable
)

// IsRetryable reports whether err is worth repeating.
//
// A connection that went away, a server that is still starting up or a
// lock that was contended are worth another attempt. A constraint
// violation is not: the same statement fails the same way every time, and
// retrying it turns a permanent mistake into a hot loop.
func IsRetryable(err error) bool {
	return classify(err) != failurePermanent
}

// IsUnavailable reports whether err says the database could not be reached.
//
// It is narrower than IsRetryable on purpose: a deadlock is a temporary
// problem between two sessions, not an unreachable database, and it must
// not take the instance out of rotation.
func IsUnavailable(err error) bool {
	return classify(err) == failureUnavailable
}

// classify decides what an error means. The order matters: a context error
// is the caller's own deadline and no server state, and a SQLSTATE is the
// database's own verdict, so both are read before the generic network
// checks.
func classify(err error) failureKind {
	if err == nil {
		return failurePermanent
	}
	if errors.Is(err, ErrUnavailable) {
		return failureUnavailable
	}
	// The caller's deadline is not the database's fault, and the context
	// is already done: there is nothing to wait for.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return failurePermanent
	}
	// A Postgres error carries its SQLSTATE, which is the most precise
	// answer available.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return classifySQLState(pgErr.Code)
	}
	// A connection that could not be established at all.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return failureUnavailable
	}
	// A connection that died on the way: a timeout, a refused dial, a
	// reset peer, a host that does not resolve.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return failureUnavailable
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return failureUnavailable
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return failureUnavailable
	}
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return failureUnavailable
	}
	if isTransientMessage(err.Error()) {
		return failureUnavailable
	}
	return failurePermanent
}

// classifySQLState maps a SQLSTATE to what it means for a retry.
//
// Class 08 is a connection exception: the server closed the connection, the
// connection was refused, the host vanished. The rest are the states
// Postgres uses for "not now" — a serialization failure or a deadlock, a
// server that is still starting up, one that was shut down, one that is
// over its connection limit.
//
// Class 23 (integrity constraint violation) is deliberately absent: a
// unique or foreign key violation is a permanent answer, and repeating the
// statement cannot change it. A dropped database (57P04) is permanent too:
// nothing comes back until an operator puts it back.
func classifySQLState(code string) failureKind {
	if strings.HasPrefix(code, "08") {
		return failureUnavailable
	}
	switch code {
	case "40001", // serialization_failure
		"40P01", // deadlock_detected
		"55006": // object_in_use
		return failureTransient
	case "57P01", // admin_shutdown
		"57P02", // crash_shutdown
		"57P03", // cannot_connect_now
		"53300": // too_many_connections
		return failureUnavailable
	case "57P04": // database_dropped
		return failurePermanent
	}
	return failurePermanent
}

// transientMessages are the driver-level phrases that mean "the connection
// is gone". The drivers wrap some of these in plain errors rather than in a
// typed one, so the message is the only signal left. The list is short on
// purpose: a loose match on the word "timeout" would classify a slow query
// as an outage.
var transientMessages = []string{
	"conn closed",
	"closed pool",
	"connection refused",
	"connection reset by peer",
	"broken pipe",
	"no such host",
	"i/o timeout",
	"server closed the connection unexpectedly",
	"unexpected eof",
	"failed to connect",
	"the database system is starting up",
	"the database system is in recovery mode",
	"too many connections",
	"too many clients",
	"cannot connect now",
	"terminating connection due to administrator command",
}

// isTransientMessage reports whether the message names a lost connection.
func isTransientMessage(msg string) bool {
	lower := strings.ToLower(msg)
	for _, phrase := range transientMessages {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// RetryPolicy is how a transient failure is repeated.
type RetryPolicy struct {
	// Attempts is the total number of attempts, the first one included.
	// Zero or one means "try once".
	Attempts int
	// Base is the delay before the second attempt. It doubles after every
	// failed attempt, up to Max.
	Base time.Duration
	// Max caps a single delay. Zero means no cap.
	Max time.Duration
	// Jitter randomises each delay within the upper half of the range, so a
	// fleet of instances that loses the same database at the same moment
	// does not come back in lockstep.
	Jitter bool
	// Rand is the randomness source. Nil uses the global one; a test sets
	// it to make the delays deterministic.
	Rand func() float64
}

// ConnectPolicy returns the policy used while reaching the database at
// startup.
func ConnectPolicy(attempts int, delay time.Duration) RetryPolicy {
	if attempts <= 0 {
		attempts = DefaultConnectAttempts
	}
	if delay <= 0 {
		delay = DefaultConnectDelay
	}
	return RetryPolicy{
		Attempts: attempts,
		Base:     delay,
		Max:      MaxConnectDelay,
		Jitter:   true,
	}
}

// ReadPolicy returns the policy used for reads that failed transiently.
func ReadPolicy() RetryPolicy {
	return RetryPolicy{
		Attempts: DefaultReadAttempts,
		Base:     DefaultReadDelay,
		Max:      MaxReadDelay,
		Jitter:   true,
	}
}

// Do runs fn until it succeeds, until its error turns out to be permanent,
// or until the attempts are used up.
//
// The error of the last attempt is returned, so the caller can report why
// the operation failed. A cancelled context stops the loop at once: there
// is no point in waiting out a backoff for a request nobody is waiting for.
func (p RetryPolicy) Do(ctx context.Context, fn func(context.Context) error) error {
	attempts := p.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = fn(ctx)
		if err == nil {
			return nil
		}
		if !IsRetryable(err) {
			return err
		}
		if attempt == attempts-1 {
			break
		}
		if waitErr := sleep(ctx, p.delay(attempt)); waitErr != nil {
			return waitErr
		}
	}
	return err
}

// delay returns how long to wait before the next attempt. attempt is
// zero-based: the wait after the first failure is the base delay.
func (p RetryPolicy) delay(attempt int) time.Duration {
	d := p.Base
	if d <= 0 {
		return 0
	}
	for i := 0; i < attempt; i++ {
		d *= 2
		if p.Max > 0 && d >= p.Max {
			d = p.Max
			break
		}
	}
	if p.Max > 0 && d > p.Max {
		d = p.Max
	}
	if p.Jitter {
		d = jitter(d, p.Rand)
	}
	return d
}

// jitter spreads a delay over its upper half, so two instances that failed
// at the same moment do not retry at the same moment.
func jitter(d time.Duration, randFloat func() float64) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	if randFloat == nil {
		randFloat = rand.Float64
	}
	return half + time.Duration(randFloat()*float64(d-half))
}

// sleep waits for d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DegradationReporter is implemented by a store that knows whether its
// database is currently unreachable.
//
// It is deliberately a separate interface from Store: the SQLite backend
// has no connection to lose, and every test fake would otherwise have to
// answer for a condition that does not apply to it.
type DegradationReporter interface {
	// Degraded reports whether the database was unreachable the last time
	// the store talked to it.
	Degraded() bool
}

// DatabaseHealth tracks whether the database is reachable and logs the
// transition into and out of that state once — not on every attempt.
//
// A service that loses its database for thirty seconds should say so once
// and then get back to work. A line per failed query would bury the rest
// of the log exactly when an operator is reading it, and the recovery
// would be lost in the noise of the failures that preceded it.
type DatabaseHealth struct {
	mu       sync.Mutex
	degraded bool
	since    time.Time
	log      *slog.Logger
	now      func() time.Time
}

// NewDatabaseHealth returns a tracker that logs through log. A nil logger
// uses the default one.
func NewDatabaseHealth(log *slog.Logger) *DatabaseHealth {
	if log == nil {
		log = slog.Default()
	}
	return &DatabaseHealth{log: log, now: time.Now}
}

// Observe records the outcome of one database operation.
//
// A failure that says the database is unreachable moves the tracker into
// the degraded state; any success moves it back out. An error that is
// transient but not an outage — a deadlock, a serialization failure — is
// not an outage and changes nothing.
func (h *DatabaseHealth) Observe(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case err != nil && IsUnavailable(err):
		if h.degraded {
			// Already degraded: the transition was logged, and one line
			// per attempt is the noise this tracker exists to avoid.
			return
		}
		h.degraded = true
		h.since = h.now()
		h.log.Warn("database unavailable", "since", h.since.UTC().Format(time.RFC3339), "err", err)
	case err == nil:
		if !h.degraded {
			return
		}
		outage := h.now().Sub(h.since)
		h.degraded = false
		h.since = time.Time{}
		h.log.Warn("database reachable again", "outage", outage.Round(time.Second).String())
	}
}

// Degraded reports whether the database was unreachable the last time the
// store talked to it.
func (h *DatabaseHealth) Degraded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.degraded
}

// DegradedSince reports when the database became unreachable, and whether it
// is unreachable now.
func (h *DatabaseHealth) DegradedSince() (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.since, h.degraded
}
