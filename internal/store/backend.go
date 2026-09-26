package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Scheme returns the lower-cased scheme of a DATABASE_URL. It is exported so
// callers that need to branch on the backend (for example config validation)
// share one parser with the store constructor.
func Scheme(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if u.Scheme == "" {
		return "", fmt.Errorf("DATABASE_URL is not a parseable URL (supported schemes: postgres, postgresql, sqlite)")
	}
	return strings.ToLower(u.Scheme), nil
}

// BackendName returns a short, log-safe name for the backend that
// databaseURL selects: "postgres", "sqlite" or "unknown". It never includes
// credentials, so it is safe to put on a startup log line.
func BackendName(databaseURL string) string {
	scheme, err := Scheme(databaseURL)
	if err != nil {
		return "unknown"
	}
	switch scheme {
	case "postgres", "postgresql":
		return "postgres"
	case "sqlite":
		return "sqlite"
	default:
		return "unknown"
	}
}

// New connects to the database named by databaseURL and returns the Store
// implementation its scheme selects: postgres/postgresql for the pgx pool,
// sqlite for a single-file database. cipher, when non-nil, encrypts
// channels.config at rest; both backends share the same envelope.
//
// The caller is expected to have run Migrate first, matching the previous
// NewPostgres contract. settings only apply to the Postgres pool.
func New(ctx context.Context, databaseURL string, settings PoolSettings, cipher ConfigCipher) (Store, error) {
	scheme, err := Scheme(databaseURL)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case "postgres", "postgresql":
		p, err := NewPostgres(ctx, databaseURL, settings)
		if err != nil {
			return nil, err
		}
		return p.WithConfigCipher(cipher), nil
	case "sqlite":
		s, err := NewSQLite(ctx, databaseURL)
		if err != nil {
			return nil, err
		}
		return s.WithConfigCipher(cipher), nil
	default:
		return nil, fmt.Errorf("unsupported DATABASE_URL scheme %q (supported schemes: postgres, postgresql, sqlite)", scheme)
	}
}
