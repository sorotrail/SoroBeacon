-- Bounded archive of every ingested contract event, so a rule can be
-- replayed against the real event corpus instead of the biased set of
-- events that already matched some rule (see internal/replay).
--
-- SQLite version: JSONB becomes TEXT holding JSON, timestamptz becomes
-- TEXT in the fixed-width UTC layout every time column in this schema
-- uses. Retention is bounded (default 30 days, EVENT_ARCHIVE_RETENTION).
CREATE TABLE event_archive (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    network          TEXT    NOT NULL DEFAULT '',
    event_id         TEXT    NOT NULL,
    contract_id      TEXT    NOT NULL,
    ledger           INTEGER NOT NULL,
    ledger_closed_at TEXT,
    tx_hash          TEXT    NOT NULL DEFAULT '',
    event_name       TEXT    NOT NULL DEFAULT '',
    topics           TEXT    NOT NULL DEFAULT '[]',
    value            TEXT,
    fields           TEXT,
    ingested_at      TEXT    NOT NULL,
    UNIQUE (network, event_id)
);

-- Replay asks for one contract's events in a ledger range; this is the
-- index that answers it without scanning the archive.
CREATE INDEX event_archive_contract_ledger_idx ON event_archive (contract_id, ledger);

-- The pruner cuts by age.
CREATE INDEX event_archive_ingested_idx ON event_archive (ingested_at);
