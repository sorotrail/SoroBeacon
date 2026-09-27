-- Reorg detection state: the hash of every recently ingested ledger. The
-- poller re-reads these each cycle and treats a changed hash as a chain
-- reorganisation, retracting the alerts derived from the orphaned ledgers.
-- The window is pruned by the poller; only `ledger` and `hash` are load
-- bearing, and observed_at exists so an operator can see how stale the window
-- is when debugging.
CREATE TABLE ledger_hashes (
    ledger      BIGINT PRIMARY KEY,
    hash        TEXT        NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ledger_hashes_observed_idx ON ledger_hashes (observed_at);

-- The ledger an alert came from, stored as a column rather than parsed out of
-- the JSON payload so the retraction update is a plain indexed comparison.
-- Existing rows default to 0 and can never be retracted, which is correct:
-- they predate reorg tracking.
ALTER TABLE alerts ADD COLUMN ledger BIGINT NOT NULL DEFAULT 0;

-- When set, the ledger this alert came from was orphaned by a reorg. The row
-- is kept because a delivered notification cannot be unsent; this column is
-- how the API and dashboard stop presenting it as canonical.
ALTER TABLE alerts ADD COLUMN retracted_at TIMESTAMPTZ;

CREATE INDEX alerts_ledger_idx ON alerts (ledger);
