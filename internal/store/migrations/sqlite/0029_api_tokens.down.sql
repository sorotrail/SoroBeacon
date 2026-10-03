-- Reverses the SQLite 0014. See the Postgres file for why a downgrade drops the
-- tokens rather than keeping them unscoped.
DROP TABLE IF EXISTS api_tokens;
