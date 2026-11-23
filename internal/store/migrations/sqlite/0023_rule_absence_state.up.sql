-- Parity with the Postgres 0023. BIGINT becomes INTEGER and TIMESTAMPTZ
-- becomes TEXT in the fixed 'YYYY-MM-DDTHH:MM:SS.mmmZ' format every other
-- SQLite timestamp column uses, so lexicographic order equals chronological
-- order and GREATEST can be written as MAX in the upsert.
CREATE TABLE rule_absence_state (
    rule_id      INTEGER NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    event_name   TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (rule_id, event_name)
);
