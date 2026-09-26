-- SQLite equivalent of the Postgres 0001_init. The Postgres DDL does not run
-- unmodified: JSONB becomes TEXT (JSON is stored as text and queried with
-- json_extract), TIMESTAMPTZ becomes TEXT in a fixed
-- 'YYYY-MM-DDTHH:MM:SS.mmmZ' format so lexicographic ordering equals
-- chronological ordering, and BOOLEAN becomes INTEGER 0/1. AUTOINCREMENT
-- keeps ids monotonic across deletes, which the alerts keyset pagination
-- relies on. Timestamp defaults use strftime('%f') to match that fixed
-- format written by the Go layer.
CREATE TABLE monitors (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    contract_ids TEXT    NOT NULL DEFAULT '[]',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE rules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    type       TEXT    NOT NULL,
    params     TEXT    NOT NULL DEFAULT '{}',
    enabled    INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX rules_monitor_id_idx ON rules (monitor_id);

-- config holds channel secrets (webhook URLs, tokens, SMTP credentials). On
-- SQLite it is often a local file on a Raspberry Pi, so at-rest protection is
-- the filesystem's job; set CONFIG_ENCRYPTION_KEY to also encrypt the column.
CREATE TABLE channels (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    type       TEXT    NOT NULL,
    config     TEXT    NOT NULL DEFAULT '{}',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE monitor_channels (
    monitor_id INTEGER NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    channel_id INTEGER NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    PRIMARY KEY (monitor_id, channel_id)
);

CREATE TABLE alerts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    rule_id    INTEGER NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    event_id   TEXT    NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '{}',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Dedup guard: the same rule can only ever fire once per source event.
CREATE UNIQUE INDEX alerts_rule_event_uidx ON alerts (rule_id, event_id);
CREATE INDEX alerts_monitor_created_idx ON alerts (monitor_id, created_at DESC);

CREATE TABLE delivery_attempts (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    alert_id         INTEGER NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    channel_id       INTEGER NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    status           TEXT    NOT NULL,
    response_snippet TEXT    NOT NULL DEFAULT '',
    attempted_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX delivery_attempts_alert_idx ON delivery_attempts (alert_id);

-- Single-row poller checkpoint.
CREATE TABLE ingest_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    last_ledger INTEGER NOT NULL DEFAULT 0,
    last_cursor TEXT    NOT NULL DEFAULT '',
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO ingest_state (id) VALUES (1);
