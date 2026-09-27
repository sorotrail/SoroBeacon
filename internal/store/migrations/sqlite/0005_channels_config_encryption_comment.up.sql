-- Parity with the Postgres 0005 migration, which only documents that
-- channels.config is encrypted by the application (CONFIG_ENCRYPTION_KEY),
-- not by the column type. SQLite has no COMMENT ON statement and the column
-- needs no schema change, so this migration is intentionally a no-op. It
-- exists so version numbers stay aligned with the Postgres set.
SELECT 1;
