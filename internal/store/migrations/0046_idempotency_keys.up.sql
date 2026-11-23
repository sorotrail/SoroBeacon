-- Per-workspace idempotency key records for write endpoints.
-- Keys are scoped by workspace_id so tenants cannot collide or read each
-- other's cached responses. Only a SHA-256 digest of the request is stored
-- (never raw request bodies), so channel secrets are never persisted here.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    workspace_id    TEXT        NOT NULL DEFAULT 'default',
    idempotency_key TEXT        NOT NULL,
    method          TEXT        NOT NULL,
    path            TEXT        NOT NULL,
    request_hash    TEXT        NOT NULL,
    status_code     INTEGER     NOT NULL DEFAULT 0,
    content_type    TEXT        NOT NULL DEFAULT '',
    response_body   TEXT        NOT NULL DEFAULT '',
    completed       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idempotency_keys_expires_at_idx
    ON idempotency_keys (expires_at);
