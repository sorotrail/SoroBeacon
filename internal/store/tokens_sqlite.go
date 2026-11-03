package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

// The SQLite half of API-token storage. It mirrors the Postgres methods in
// postgres.go statement for statement; the differences are the ones SQLite
// forces — positional placeholders, and the three nullable timestamps stored
// as TEXT in the fixed layout rather than as TIMESTAMPTZ.
//
// Every method scopes itself to ctx's workspace except TokenByHash, which runs
// before tenancy is known: the row it finds is what decides the workspace.

// CreateAPIToken inserts one token row. The caller passes a digest, never a
// secret: auth.Manager hashes before this point and drops the plaintext, so the
// string that reaches SQL (or a log, or an error) is already one-way.
func (s *SQLite) CreateAPIToken(ctx context.Context, t *auth.Token) error {
	var expiresAt any
	if !t.ExpiresAt.IsZero() {
		expiresAt = sqliteTimeString(t.ExpiresAt)
	}
	ws := workspaceID(ctx)
	var created string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO api_tokens (workspace_id, name, token_hash, prefix, scopes, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?) RETURNING id, created_at`,
		ws, t.Name, t.Hash, t.Prefix, auth.JoinScopes(t.Scopes), expiresAt,
	).Scan(&t.ID, &created)
	if err != nil {
		return mapSQLiteErr(err)
	}
	if t.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return err
	}
	t.Workspace = ws
	return nil
}

// TokenByHash is the authentication read, and the one token method that does
// not scope itself to ctx's workspace: a request carrying a token has no tenant
// until this row answers, and the row's own workspace_id is what the request
// then gets — never anything the caller supplied.
func (s *SQLite) TokenByHash(ctx context.Context, hash string) (*auth.Token, bool, error) {
	t, err := scanSQLiteAPIToken(s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at,
		        rotated_from_id, successor_id, rotated_at, pre_rotation_expires_at
		   FROM api_tokens WHERE token_hash = ?`, hash))
	if err != nil {
		if err == ErrNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &t, true, nil
}

func (s *SQLite) ListAPITokens(ctx context.Context) ([]auth.Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at,
		        rotated_from_id, successor_id, rotated_at, pre_rotation_expires_at
		   FROM api_tokens WHERE workspace_id = ? ORDER BY id DESC`, workspaceID(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []auth.Token
	for rows.Next() {
		t, err := scanSQLiteAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetAPIToken returns one token by id in the caller's workspace.
func (s *SQLite) GetAPIToken(ctx context.Context, id int64) (*auth.Token, error) {
	t, err := scanSQLiteAPIToken(s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at,
		        rotated_from_id, successor_id, rotated_at, pre_rotation_expires_at
		   FROM api_tokens WHERE id = ? AND workspace_id = ?`, id, workspaceID(ctx)))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// RotateAPIToken atomically creates a successor token and updates the predecessor
// with the successor's id, rotation timestamp, and new expiry.
func (s *SQLite) RotateAPIToken(ctx context.Context, predecessorID int64, successor *auth.Token, predecessorExpiry time.Time) error {
	ws := workspaceID(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	pred, err := scanSQLiteAPIToken(tx.QueryRowContext(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at,
		        rotated_from_id, successor_id, rotated_at, pre_rotation_expires_at
		   FROM api_tokens WHERE id = ? AND workspace_id = ?`, predecessorID, ws))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if !pred.RevokedAt.IsZero() {
		return fmt.Errorf("cannot rotate a revoked token")
	}
	if !pred.ExpiresAt.IsZero() && now.After(pred.ExpiresAt) {
		return fmt.Errorf("cannot rotate an expired token")
	}

	var succExpires any
	if !successor.ExpiresAt.IsZero() {
		succExpires = sqliteTimeString(successor.ExpiresAt)
	}

	nowStr := sqliteTimeString(now)
	var succCreated, succRotated string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO api_tokens (workspace_id, name, token_hash, prefix, scopes, expires_at, rotated_from_id, rotated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, created_at, rotated_at`,
		ws, successor.Name, successor.Hash, successor.Prefix, auth.JoinScopes(successor.Scopes),
		succExpires, predecessorID, nowStr).Scan(&successor.ID, &succCreated, &succRotated)
	if err != nil {
		return mapSQLiteErr(err)
	}
	successor.Workspace = ws
	if successor.CreatedAt, err = parseSQLiteTime(succCreated); err != nil {
		return err
	}
	pRot, err := parseSQLiteTimePtr(sql.NullString{String: succRotated, Valid: succRotated != ""})
	if err != nil {
		return err
	}
	if pRot != nil {
		successor.RotatedAt = *pRot
	}
	successor.RotatedFromID = &predecessorID

	var predExpiry any
	if !predecessorExpiry.IsZero() {
		predExpiry = sqliteTimeString(predecessorExpiry)
	}
	var origPredExpires any
	if !pred.ExpiresAt.IsZero() {
		origPredExpires = sqliteTimeString(pred.ExpiresAt)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE api_tokens
		    SET successor_id = ?, rotated_at = ?, pre_rotation_expires_at = ?, expires_at = ?
		  WHERE id = ? AND workspace_id = ?`,
		successor.ID, nowStr, origPredExpires, predExpiry, predecessorID, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}

	return tx.Commit()
}

// AbandonAPITokenRotation revokes the successor and restores predecessor's pre-rotation expiry.
func (s *SQLite) AbandonAPITokenRotation(ctx context.Context, id int64) error {
	ws := workspaceID(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	tok, err := scanSQLiteAPIToken(tx.QueryRowContext(ctx,
		`SELECT id, workspace_id, name, token_hash, prefix, scopes, expires_at, last_used_at, revoked_at, created_at,
		        rotated_from_id, successor_id, rotated_at, pre_rotation_expires_at
		   FROM api_tokens WHERE id = ? AND workspace_id = ?`, id, ws))
	if err != nil {
		return err
	}

	var predID, succID int64
	switch {
	case tok.SuccessorID != nil:
		predID = tok.ID
		succID = *tok.SuccessorID
	case tok.RotatedFromID != nil:
		succID = tok.ID
		predID = *tok.RotatedFromID
	default:
		return fmt.Errorf("no rotation in progress for token %d", id)
	}

	var preRotExpires sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT pre_rotation_expires_at FROM api_tokens WHERE id = ? AND workspace_id = ?`,
		predID, ws).Scan(&preRotExpires)
	if err != nil {
		return mapSQLiteErr(err)
	}

	nowStr := sqliteTimeString(time.Now())
	_, err = tx.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND workspace_id = ?`,
		nowStr, succID, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}

	var restoredExpiry any
	if preRotExpires.Valid {
		restoredExpiry = preRotExpires.String
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE api_tokens
		    SET successor_id = NULL, rotated_at = NULL, pre_rotation_expires_at = NULL, expires_at = ?
		  WHERE id = ? AND workspace_id = ?`,
		restoredExpiry, predID, ws)
	if err != nil {
		return mapSQLiteErr(err)
	}

	return tx.Commit()
}

// RevokeAPIToken retires one of the caller's tokens. COALESCE keeps the first
// revocation timestamp, so revoking twice is a no-op that still reports success
// rather than a 404 for a token the caller can already see is revoked; a row in
// another workspace is ErrNotFound, the same answer a missing id gives.
func (s *SQLite) RevokeAPIToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND workspace_id = ?`,
		sqliteTimeString(time.Now()), id, workspaceID(ctx))
	if err != nil {
		return mapSQLiteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAPIToken records that a token authenticated. The write is unconditional
// because the caller throttles it (auth.Manager skips a token used within the
// last minute); a store-side guard would make the throttle's interval a second
// copy of the same constant.
func (s *SQLite) TouchAPIToken(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ? AND workspace_id = ?`,
		sqliteTimeString(at), id, workspaceID(ctx))
	return err
}

// scanSQLiteAPIToken reads one api_tokens row in the column order the
// single-row read and the listing share. The three nullable timestamps come
// back as NULL-able strings and become zero times, which is what auth.Token's
// Live and the dashboard's "never used" both key off.
func scanSQLiteAPIToken(r rowScanner) (auth.Token, error) {
	var t auth.Token
	var scopes string
	var expiresAt, lastUsedAt, revokedAt sql.NullString
	var created string
	var rotatedFromID, successorID sql.NullInt64
	var rotatedAt, preRotExpires sql.NullString
	if err := r.Scan(&t.ID, &t.Workspace, &t.Name, &t.Hash, &t.Prefix, &scopes,
		&expiresAt, &lastUsedAt, &revokedAt, &created,
		&rotatedFromID, &successorID, &rotatedAt, &preRotExpires); err != nil {
		return t, mapSQLiteErr(err)
	}
	t.Scopes = auth.SplitScopes(scopes)

	var err error
	if t.CreatedAt, err = parseSQLiteTime(created); err != nil {
		return t, err
	}
	for _, f := range []struct {
		raw sql.NullString
		dst *time.Time
	}{
		{expiresAt, &t.ExpiresAt},
		{lastUsedAt, &t.LastUsedAt},
		{revokedAt, &t.RevokedAt},
		{rotatedAt, &t.RotatedAt},
		{preRotExpires, &t.PreRotationExpiresAt},
	} {
		p, err := parseSQLiteTimePtr(f.raw)
		if err != nil {
			return t, err
		}
		if p != nil {
			*f.dst = *p
		}
	}
	if rotatedFromID.Valid {
		v := rotatedFromID.Int64
		t.RotatedFromID = &v
	}
	if successorID.Valid {
		v := successorID.Int64
		t.SuccessorID = &v
	}
	return t, nil
}
