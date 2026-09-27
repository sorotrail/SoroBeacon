-- Digest mode batches a channel's alerts into one summary per window.
-- digest_mode '' (the default) delivers immediately, so existing channels are
-- untouched; 'window' accumulates for digest_window_seconds.
ALTER TABLE channels
    ADD COLUMN digest_mode TEXT NOT NULL DEFAULT '',
    ADD COLUMN digest_window_seconds BIGINT NOT NULL DEFAULT 0;

-- Alerts awaiting a digest flush. Rows are persisted so a restart does not
-- silently drop a partial window, and they cascade when the channel goes.
CREATE TABLE pending_digests (
    id         BIGSERIAL PRIMARY KEY,
    channel_id BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX pending_digests_channel_idx ON pending_digests (channel_id, id);
