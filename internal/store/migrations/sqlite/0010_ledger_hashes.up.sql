-- SQLite equivalent of the Postgres 0010_ledger_hashes. The hash window is a
-- single-file database too, so the same table ships here; INTEGER/TEXT replace
-- BIGINT/TIMESTAMPTZ and the timestamp default uses the fixed strftime shape
-- so it compares chronologically as text.
CREATE TABLE ledger_hashes (
    ledger      INTEGER PRIMARY KEY,
    hash        TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX ledger_hashes_observed_idx ON ledger_hashes (observed_at);

ALTER TABLE alerts ADD COLUMN ledger INTEGER NOT NULL DEFAULT 0;
ALTER TABLE alerts ADD COLUMN retracted_at TEXT;

CREATE INDEX alerts_ledger_idx ON alerts (ledger);
