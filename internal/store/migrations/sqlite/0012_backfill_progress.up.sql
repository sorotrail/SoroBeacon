-- SQLite equivalent of the Postgres 0012_backfill_progress. Backfilled alerts
-- come from a historical replay (internal/backfill), not live ingestion, and
-- are flagged so an operator can tell them apart. Existing rows are live
-- alerts, so the default of 0 is correct.
ALTER TABLE alerts ADD COLUMN backfilled INTEGER NOT NULL DEFAULT 0;

-- Progress for the opt-in historical replay, one row per monitor. next_ledger
-- and cursor are the source's resume point, written as pages are consumed, so
-- an interrupted backfill picks up where it stopped instead of replaying from
-- the start. complete marks a finished run; a later run for the same monitor
-- replaces the row. INTEGER/TEXT replace BIGINT/TIMESTAMPTZ and the timestamp
-- default uses the fixed strftime shape so it compares chronologically as text.
CREATE TABLE backfills (
    monitor_id  INTEGER PRIMARY KEY REFERENCES monitors (id) ON DELETE CASCADE,
    from_ledger INTEGER NOT NULL,
    to_ledger   INTEGER NOT NULL,
    next_ledger INTEGER NOT NULL,
    cursor      TEXT    NOT NULL DEFAULT '',
    deliver     INTEGER NOT NULL DEFAULT 0,
    complete    INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
