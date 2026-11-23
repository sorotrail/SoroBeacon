package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// staleInProgressLease bounds how long an uncompleted idempotency claim may
// block retries if the process holding it crashed before completing or
// deleting the row.
const staleInProgressLease = 2 * time.Minute

// HashIdempotentRequest computes the canonical SHA-256 hex digest of an HTTP
// write request's method, path (including query string), and raw body bytes.
// Only the digest is stored in idempotency_keys so sensitive payloads (such as
// channel webhook URLs or bot tokens) are never persisted in the table.
func HashIdempotentRequest(method, path string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.ToUpper(strings.TrimSpace(method))))
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write([]byte(path))
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func resolveIdempotencyWorkspace(ctx context.Context) string {
	ws := string(workspaceID(ctx))
	if ws == "" {
		ws = string(workspace.Default)
	}
	return ws
}

func normalizeIdempotencyTimes(now, expiresAt time.Time) (time.Time, time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if expiresAt.IsZero() || !expiresAt.After(now) {
		expiresAt = now.Add(DefaultIdempotencyTTL)
	} else {
		expiresAt = expiresAt.UTC()
	}
	return now, expiresAt
}

func evaluateExistingIdempotencyRecord(existing *IdempotencyRecord, method, path, requestHash string) (IdempotencyClaimOutcome, *IdempotencyRecord, error) {
	if existing.Method != method || existing.Path != path || existing.RequestHash != requestHash {
		return IdempotencyConflict, existing, ErrIdempotencyKeyConflict
	}
	if existing.Completed {
		return IdempotencyReplayed, existing, nil
	}
	return IdempotencyInProgress, existing, nil
}

// SafeClaimIdempotencyKey invokes st.ClaimIdempotencyKey and reports supported=false
// when st is a test fake that embeds a nil store.Store.
func SafeClaimIdempotencyKey(
	ctx context.Context,
	st Idempotency,
	key, method, path, requestHash string,
	now, expiresAt time.Time,
) (outcome IdempotencyClaimOutcome, rec *IdempotencyRecord, supported bool, err error) {
	if st == nil {
		return "", nil, false, nil
	}
	supported = true
	defer func() {
		if r := recover(); r != nil {
			outcome = ""
			rec = nil
			supported = false
			err = nil
		}
	}()
	outcome, rec, err = st.ClaimIdempotencyKey(ctx, key, method, path, requestHash, now, expiresAt)
	return outcome, rec, supported, err
}

// SafeCompleteIdempotencyKey invokes st.CompleteIdempotencyKey while tolerating
// test fakes that embed a nil store.Store.
func SafeCompleteIdempotencyKey(
	ctx context.Context,
	st Idempotency,
	key, requestHash string,
	statusCode int,
	contentType string,
	responseBody []byte,
	expiresAt time.Time,
) (err error) {
	if st == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = nil
		}
	}()
	return st.CompleteIdempotencyKey(ctx, key, requestHash, statusCode, contentType, responseBody, expiresAt)
}

// SafeGetIdempotencyKey invokes st.GetIdempotencyKey while tolerating test
// fakes that embed a nil store.Store.
func SafeGetIdempotencyKey(ctx context.Context, st Idempotency, key string, now time.Time) (rec *IdempotencyRecord, err error) {
	if st == nil {
		return nil, ErrNotFound
	}
	defer func() {
		if r := recover(); r != nil {
			rec = nil
			err = ErrNotFound
		}
	}()
	return st.GetIdempotencyKey(ctx, key, now)
}

// SafeDeleteIdempotencyKey invokes st.DeleteIdempotencyKey while tolerating
// test fakes that embed a nil store.Store.
func SafeDeleteIdempotencyKey(ctx context.Context, st Idempotency, key string) (err error) {
	if st == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = nil
		}
	}()
	return st.DeleteIdempotencyKey(ctx, key)
}

// --- Postgres implementation ---

func (p *Postgres) ClaimIdempotencyKey(
	ctx context.Context,
	key, method, path, requestHash string,
	now, expiresAt time.Time,
) (IdempotencyClaimOutcome, *IdempotencyRecord, error) {
	ws := resolveIdempotencyWorkspace(ctx)
	key = strings.TrimSpace(key)
	method = strings.ToUpper(strings.TrimSpace(method))
	now, expiresAt = normalizeIdempotencyTimes(now, expiresAt)
	staleBefore := now.Add(-staleInProgressLease)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", nil, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// Remove an expired record (or an abandoned in-progress record from a crashed
	// process) for this (workspace_id, idempotency_key) before attempting the insert.
	if _, err := tx.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE workspace_id = $1
		  AND idempotency_key = $2
		  AND (expires_at <= $3 OR (completed = FALSE AND created_at <= $4))`,
		ws, key, now, staleBefore,
	); err != nil {
		return "", nil, mapErr(err)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (
			workspace_id, idempotency_key, method, path, request_hash,
			status_code, content_type, response_body, completed, created_at, expires_at
		) VALUES ($1, $2, $3, $4, $5, 0, '', '', FALSE, $6, $7)
		ON CONFLICT (workspace_id, idempotency_key) DO NOTHING`,
		ws, key, method, path, requestHash, now, expiresAt,
	)
	if err != nil {
		return "", nil, mapErr(err)
	}

	if tag.RowsAffected() == 1 {
		if err := tx.Commit(ctx); err != nil {
			return "", nil, mapErr(err)
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

	var existing IdempotencyRecord
	var bodyStr string
	err = tx.QueryRow(ctx, `
		SELECT workspace_id, idempotency_key, method, path, request_hash,
		       status_code, content_type, response_body, completed, created_at, expires_at
		FROM idempotency_keys
		WHERE workspace_id = $1 AND idempotency_key = $2`,
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
		&existing.Completed,
		&existing.CreatedAt,
		&existing.ExpiresAt,
	)
	if err != nil {
		return "", nil, mapErr(err)
	}
	existing.ResponseBody = []byte(bodyStr)

	if err := tx.Commit(ctx); err != nil {
		return "", nil, mapErr(err)
	}
	return evaluateExistingIdempotencyRecord(&existing, method, path, requestHash)
}

func (p *Postgres) CompleteIdempotencyKey(
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

	tag, err := p.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET status_code   = $1,
		    content_type  = $2,
		    response_body = $3,
		    completed     = TRUE,
		    expires_at    = $4
		WHERE workspace_id = $5
		  AND idempotency_key = $6
		  AND request_hash = $7`,
		statusCode, contentType, string(responseBody), expiresAt, ws, key, requestHash,
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) GetIdempotencyKey(ctx context.Context, key string, now time.Time) (*IdempotencyRecord, error) {
	ws := resolveIdempotencyWorkspace(ctx)
	key = strings.TrimSpace(key)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	var rec IdempotencyRecord
	var bodyStr string
	err := p.pool.QueryRow(ctx, `
		SELECT workspace_id, idempotency_key, method, path, request_hash,
		       status_code, content_type, response_body, completed, created_at, expires_at
		FROM idempotency_keys
		WHERE workspace_id = $1
		  AND idempotency_key = $2
		  AND expires_at > $3`,
		ws, key, now,
	).Scan(
		&rec.WorkspaceID,
		&rec.IdempotencyKey,
		&rec.Method,
		&rec.Path,
		&rec.RequestHash,
		&rec.StatusCode,
		&rec.ContentType,
		&bodyStr,
		&rec.Completed,
		&rec.CreatedAt,
		&rec.ExpiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, mapErr(err)
	}
	rec.ResponseBody = []byte(bodyStr)
	return &rec, nil
}

func (p *Postgres) DeleteIdempotencyKey(ctx context.Context, key string) error {
	ws := resolveIdempotencyWorkspace(ctx)
	key = strings.TrimSpace(key)
	_, err := p.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE workspace_id = $1 AND idempotency_key = $2`,
		ws, key,
	)
	return mapErr(err)
}

func (p *Postgres) PruneExpiredIdempotencyKeys(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		before = time.Now().UTC()
	} else {
		before = before.UTC()
	}
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE expires_at <= $1`,
		before,
	)
	if err != nil {
		return 0, mapErr(err)
	}
	return tag.RowsAffected(), nil
}
