-- Backfilled alerts come from a historical replay (internal/backfill), not
-- live ingestion, and are flagged so an operator can tell them apart. Existing
-- rows are live alerts, so the default of false is correct.
ALTER TABLE alerts ADD COLUMN backfilled BOOLEAN NOT NULL DEFAULT FALSE;

-- Progress for the opt-in historical replay, one row per monitor. next_ledger
-- and cursor are the source's resume point, written as pages are consumed, so
-- an interrupted backfill picks up where it stopped instead of replaying from
-- the start. complete marks a finished run; a later run for the same monitor
-- replaces the row.
CREATE TABLE backfills (
    monitor_id  BIGINT      PRIMARY KEY REFERENCES monitors (id) ON DELETE CASCADE,
    from_ledger BIGINT      NOT NULL,
    to_ledger   BIGINT      NOT NULL,
    next_ledger BIGINT      NOT NULL,
    cursor      TEXT        NOT NULL DEFAULT '',
    deliver     BOOLEAN     NOT NULL DEFAULT FALSE,
    complete    BOOLEAN     NOT NULL DEFAULT FALSE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
