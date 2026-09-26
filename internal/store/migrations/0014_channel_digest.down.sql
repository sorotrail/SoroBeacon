DROP TABLE IF EXISTS pending_digests;

ALTER TABLE channels
    DROP COLUMN IF EXISTS digest_mode,
    DROP COLUMN IF EXISTS digest_window_seconds;
