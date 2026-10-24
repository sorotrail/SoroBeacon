-- Persistent record of a checkpoint that fell outside the source's
-- event retention window (SQLite version).
--
-- When SoroBeacon is down longer than the RPC node's retention window
-- (roughly 1-7 days), the checkpoint in ingest_state points at ledgers the
-- RPC has forgotten. The poller detects this on resume, jumps to the oldest
-- retained ledger, and writes the hole here so the data loss stays visible
-- after a restart instead of living only in a log line.
--
-- detected_at is TEXT in the fixed-width UTC layout every time column in
-- this schema uses, written by the store rather than defaulted.
CREATE TABLE retention_gaps (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    network     TEXT    NOT NULL DEFAULT '',
    from_ledger INTEGER NOT NULL,
    to_ledger   INTEGER NOT NULL,
    detected_at TEXT    NOT NULL,
    CHECK (to_ledger >= from_ledger)
);

-- The dashboard and GET /stats ask for the most recent gap, per network or
-- across the instance; the index answers both in index order.
CREATE INDEX retention_gaps_network_detected_idx ON retention_gaps (network, detected_at DESC);
