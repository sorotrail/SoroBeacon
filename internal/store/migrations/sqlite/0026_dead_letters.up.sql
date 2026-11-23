-- Parity with the Postgres 0026. BIGSERIAL becomes INTEGER PRIMARY KEY
-- AUTOINCREMENT and TIMESTAMPTZ becomes TEXT in the fixed
-- 'YYYY-MM-DDTHH:MM:SS.mmmZ' format every other SQLite timestamp column uses.
-- alert_created_at mirrors the Postgres table, where alerts is partitioned and
-- the reference has to be composite; SQLite does not partition, but carrying
-- the column keeps one set of statements for both backends.
CREATE TABLE dead_letters (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    alert_id         INTEGER NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    alert_created_at TEXT    NOT NULL,
    channel_id       INTEGER NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    last_error       TEXT    NOT NULL DEFAULT '',
    attempt_count    INTEGER NOT NULL,
    last_status      INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX dead_letters_channel_idx ON dead_letters (channel_id);
CREATE INDEX dead_letters_alert_idx ON dead_letters (alert_id);
