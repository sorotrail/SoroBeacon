-- SQLite equivalent of the Postgres 0013_networks. Same model and the same
-- reasons (see the Postgres file); the dialect differences are BIGINT → INTEGER
-- and the fixed strftime timestamp default.

ALTER TABLE monitors ADD COLUMN network TEXT NOT NULL DEFAULT '';
ALTER TABLE alerts   ADD COLUMN network TEXT NOT NULL DEFAULT '';

CREATE INDEX monitors_network_idx ON monitors (network, id DESC);
CREATE INDEX alerts_network_created_idx ON alerts (network, created_at DESC, id DESC);

CREATE TABLE network_ingest_state (
    network     TEXT NOT NULL PRIMARY KEY,
    last_ledger INTEGER NOT NULL DEFAULT 0,
    last_cursor TEXT    NOT NULL DEFAULT '',
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE network_ledger_hashes (
    network     TEXT    NOT NULL,
    ledger      INTEGER NOT NULL,
    hash        TEXT    NOT NULL,
    observed_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (network, ledger)
);

CREATE INDEX network_ledger_hashes_observed_idx ON network_ledger_hashes (observed_at);
