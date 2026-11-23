-- Channel signing keys: multiple HMAC-SHA256 signing keys per webhook channel
-- supporting zero-downtime key rotation, overlap verification, retirement, and
-- emergency revocation.
--
-- Design decisions:
--   * key_id identifies the key so consumers select the right secret directly
--     without trying each in turn.
--   * is_current marks the primary key used to sign new deliveries.
--   * status tracks the lifecycle: 'active', 'retired', or 'revoked'.
--   * secret is encrypted at rest via the store's cipher (when configured)
--     and never returned in listings or API responses after creation.

CREATE TABLE channel_signing_keys (
    id           BIGSERIAL PRIMARY KEY,
    channel_id   BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    key_id       TEXT NOT NULL,
    secret       TEXT NOT NULL,
    is_current   BOOLEAN NOT NULL DEFAULT FALSE,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE UNIQUE INDEX channel_signing_keys_channel_key_id_idx ON channel_signing_keys (channel_id, key_id);
CREATE INDEX channel_signing_keys_channel_status_idx ON channel_signing_keys (channel_id, status);
CREATE INDEX channel_signing_keys_workspace_idx ON channel_signing_keys (workspace_id, id DESC);
