package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *SQLite) ClaimIdempotencyKey(
	ctx context.Context,
	key, method, path, requestHash string,
	now, expiresAt time.Time,
) (IdempotencyClaimOutcome, *IdempotencyRecord, error) {
	ws := resolveIdempotencyWorkspace(ctx)
	key = strings.TrimSpace(key)
	method = strings.ToUpper(strings.TrimSpace(method))
	now, expiresAt = normalizeIdempotencyTimes(now, expiresAt)
	nowStr := sqliteTimeString(now)
	expiresStr := sqliteTimeString(expiresAt)
	staleBeforeStr := sqliteTimeString(now.Add(-staleInProgressLease))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, mapSQLiteErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM idempotency_keys
		WHERE workspace_id = ?
		  AND idempotency_key = ?
		  AND (expires_at <= ? OR (completed = 0 AND created_at <= ?))`,
		ws, key, nowStr, staleBeforeStr,
	); err != nil {
		return "", nil, mapSQLiteErr(err)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO idempotency_keys (
			workspace_id, idempotency_key, method, path, request_hash,
			status_code, content_type, response_body, completed, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, 0, '', '', 0, ?, ?)
		ON CONFLICT (workspace_id, idempotency_key) DO NOTHING`,
		ws, key, method, path, requestHash, nowStr, expiresStr,
	)
	if err != nil {
		return "", nil, mapSQLiteErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", nil, mapSQLiteErr(err)
	}
	if affected == 1 {
		if err := tx.Commit(); err != nil {
			return "", nil, mapSQLiteErr(err)
		}
		return IdempotencyClaimed, &IdempotencyRecord{
			WorkspaceID:    ws,
			IdempotencyKey: key,
			Method:         method,
			Path:           path,
			RequestHash:    requestHash,
			CreatedAt:      now,
			ExpiresAt:      expiresAt,
		}, nil
	}

	var (
		existing   IdempotencyRecord
		bodyStr    string
		completed  int
		createdStr string
		expStr     string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT workspace_id, idempotency_key, method, path, request_hash,
		       status_code, content_type, response_body, completed, created_at, expires_at
		FROM idempotency_keys
		WHERE workspace_id = ? AND idempotency_key = ?`,
		ws, key,
	).Scan(
		&existing.WorkspaceID,
		&existing.IdempotencyKey,
		&existing.Method,
		&existing.Path,
		&existing.RequestHash,
		&existing.StatusCode,
		&existing.ContentType,
		&bodyStr,
		&completed,
		&createdStr,
		&expStr,
	)
	if err != nil {
		return "", nil, mapSQLiteErr(err)
	}
	existing.ResponseBody = []byte(bodyStr)
	existing.Completed = completed != 0
	if existing.CreatedAt, err = parseSQLiteTime(createdStr); err != nil {
		return "", nil, err
	}
	if existing.ExpiresAt, err = parseSQLiteTime(expStr); err != nil {
		return "", nil, err
	}

	if err := tx.Commit(); err != nil {
		return "", nil, mapSQLiteErr(err)
	}
	return evaluateExistingIdempotencyRecord(&existing, method, path, requestHash)
}

func (s *SQLite) CompleteIdempotencyKey(
	ctx context.Context,
	key, requestHash string,
	statusCode int,
	contentType string,
	responseBody []byte,
	expiresAt time.Time,
) error {
	ws := resolveIdempotencyWorkspace(ctx)
	key = strings.TrimSpace(key)
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(DefaultIdempotencyTTL)
	} else {
		expiresAt = expiresAt.UTC()
	}
	expiresStr := sqliteTimeString(expiresAt)

	res, err := s.db.ExecContext(ctx, `
		UPDATE idempotency_keys
		SET status_code   = ?,
		    content_type  = ?,
		    response_body = ?,
		    completed     = 1,
		    expires_at    = ?
		WHERE workspace_id = ?
		  AND idempotency_key = ?
		  AND request_hash = ?`,
		statusCode, contentType, string(responseBody), expiresStr, ws, key, requestHash,
	)
	if err != nil {
		return mapSQLiteErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return mapSQLiteErr(err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) GetIdempotencyKey(ctx context.Context, key string, now time.Time) (*IdempotencyRecord, error) {
	ws := resolveIdempotencyWorkspace(ctx)
	key = strings.TrimSpace(key)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := sqliteTimeString(now)

	var (
		rec        IdempotencyRecord
		bodyStr    string
		completed  int
		createdStr string
		expStr     string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT workspace_id, idempotency_key, method, path, request_hash,
		       status_code, content_type, response_body, completed, created_at, expires_at
		FROM idempotency_keys
		WHERE workspace_id = ?
		  AND idempotency_key = ?
		  AND expires_at > ?`,
		ws, key, nowStr,
	).Scan(
		&rec.WorkspaceID,
		&rec.IdempotencyKey,
		&rec.Method,
		&rec.Path,
		&rec.RequestHash,
		&rec.StatusCode,
		&rec.ContentType,
		&bodyStr,
		&completed,
		&createdStr,
		&expStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, mapSQLiteErr(err)
	}
	rec.ResponseBody = []byte(bodyStr)
	rec.Completed = completed != 0
	if rec.CreatedAt, err = parseSQLiteTime(createdStr); err != nil {
		return nil, err
	}
	if rec.ExpiresAt, err = parseSQLiteTime(expStr); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *SQLite) DeleteIdempotencyKey(ctx context.Context, key string) error {
	ws := resolveIdempotencyWorkspace(ctx)
	key = strings.TrimSpace(key)
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys
		WHERE workspace_id = ? AND idempotency_key = ?`,
		ws, key,
	)
	return mapSQLiteErr(err)
}

func (s *SQLite) PruneExpiredIdempotencyKeys(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		before = time.Now().UTC()
	} else {
		before = before.UTC()
	}
	beforeStr := sqliteTimeString(before)
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys
		WHERE expires_at <= ?`,
		beforeStr,
	)
	if err != nil {
		return 0, mapSQLiteErr(err)
	}
	return res.RowsAffected()
}
