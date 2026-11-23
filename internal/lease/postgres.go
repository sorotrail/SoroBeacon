package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrNotPostgres is returned by NewPostgres when DATABASE_URL names a backend
// that has no advisory locks. Callers use SingleNode for those deployments
// instead of failing.
var ErrNotPostgres = errors.New("leader election needs a postgres DATABASE_URL (advisory locks are Postgres-only)")

// NewPostgres returns a lease backed by a Postgres advisory lock.
//
// A malformed connection string fails here, at startup; the error deliberately
// does not echo the URL, which embeds a password. The connection itself is
// opened lazily by the election loop, so a database that is down at startup
// delays leadership instead of failing the process.
func NewPostgres(databaseURL string, opts Options, log *slog.Logger) (*Lease, error) {
	d, err := newPostgresDialer(databaseURL)
	if err != nil {
		return nil, err
	}
	return newLease(d, opts, log), nil
}

// postgresDialer opens one dedicated connection straight to the database named
// by a DATABASE_URL. The lease deliberately does not borrow a connection from
// the store's pool: an advisory lock is bound to the session that took it, so
// the connection must stay out of every other query's reach for as long as the
// lock is held.
type postgresDialer struct {
	cfg *pgx.ConnConfig
}

func newPostgresDialer(databaseURL string) (*postgresDialer, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
	default:
		return nil, ErrNotPostgres
	}
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("DATABASE_URL is not a valid Postgres connection string")
	}
	return &postgresDialer{cfg: cfg}, nil
}

// Dial opens a session for the election loop.
func (p *postgresDialer) Dial(ctx context.Context) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, p.cfg)
	if err != nil {
		return nil, err
	}
	return &pgSession{conn: conn}, nil
}

// pgSession is the lease's private connection.
type pgSession struct {
	conn *pgx.Conn
}

// TryLock takes the session-level advisory lock under key without blocking.
// The session form (no _xact_ suffix) is the one that survives between
// statements: the lock is held until this session unlocks it or the session
// ends, which is exactly the lifetime a leadership term needs.
func (s *pgSession) TryLock(ctx context.Context, key int64) (bool, error) {
	var got bool
	if err := s.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		return false, err
	}
	return got, nil
}

// Renew reports whether this session is still connected and still holds an
// advisory lock. One round trip answers both: a session that has been cut off
// — the case that silently freed the lock for another instance to take —
// fails here rather than at whatever write comes next, and the pg_locks check
// catches the lock having been released by someone else. pg_locks reports only
// this backend's own locks, so the result is exactly "do I still hold the
// lock".
func (s *pgSession) Renew(ctx context.Context) (bool, error) {
	var held bool
	err := s.conn.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND pid = pg_backend_pid()
	)`).Scan(&held)
	if err != nil {
		return false, err
	}
	return held, nil
}

// Unlock releases the advisory lock explicitly, so a follower can take it
// immediately rather than waiting for this session to disappear.
func (s *pgSession) Unlock(ctx context.Context, key int64) error {
	var released bool
	if err := s.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&released); err != nil {
		return err
	}
	if !released {
		return errors.New("the advisory lock was not held")
	}
	return nil
}

// Close ends the session. A session-level advisory lock goes with it, so this
// is the backstop for a lock that could not be released explicitly.
func (s *pgSession) Close(ctx context.Context) error {
	return s.conn.Close(ctx)
}
