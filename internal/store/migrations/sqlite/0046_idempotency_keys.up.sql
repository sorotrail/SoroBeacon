-- SQLite half of 0046_idempotency_keys: per-workspace idempotency key records
-- for write endpoints.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    workspace_id    TEXT    NOT NULL DEFAULT 'default',
    idempotency_key TEXT    NOT NULL,
    method          TEXT    NOT NULL,
    path            TEXT    NOT NULL,
    request_hash    TEXT    NOT NULL,
    status_code     INTEGER NOT NULL DEFAULT 0,
    content_type    TEXT    NOT NULL DEFAULT '',
    response_body   TEXT    NOT NULL DEFAULT '',
    completed       INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at      TEXT    NOT NULL,
    PRIMARY KEY (workspace_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idempotency_keys_expires_at_idx
    ON idempotency_keys (expires_at);
