DROP TABLE IF EXISTS pending_digests;

ALTER TABLE channels DROP COLUMN digest_mode;
ALTER TABLE channels DROP COLUMN digest_window_seconds;
