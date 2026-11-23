-- Parity with the Postgres 0024. BIGSERIAL becomes INTEGER PRIMARY KEY
-- AUTOINCREMENT, JSONB becomes TEXT holding the same JSON bytes, and
-- TIMESTAMPTZ becomes TEXT in the fixed 'YYYY-MM-DDTHH:MM:SS.mmmZ' format
-- every other SQLite timestamp column uses, so the due-time comparison in
-- DueEscalations is a string comparison in the same order.
CREATE TABLE escalation_policies (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL UNIQUE REFERENCES monitors (id) ON DELETE CASCADE,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE escalation_steps (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    policy_id     INTEGER NOT NULL REFERENCES escalation_policies (id) ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    delay_seconds INTEGER NOT NULL DEFAULT 0,
    UNIQUE (policy_id, position)
);

-- ON DELETE RESTRICT is deliberate, as upstream: a channel referenced by a
-- policy step must not disappear silently and leave the escalation with
-- nowhere to go.
CREATE TABLE escalation_step_channels (
    step_id    INTEGER NOT NULL REFERENCES escalation_steps (id) ON DELETE CASCADE,
    channel_id INTEGER NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    PRIMARY KEY (step_id, channel_id)
);

-- alert_created_at mirrors the Postgres table, where alerts is partitioned by
-- created_at and the reference has to be composite. SQLite does not partition,
-- but carrying the column keeps one set of statements for both backends.
CREATE TABLE alert_escalations (
    alert_id         INTEGER PRIMARY KEY REFERENCES alerts (id) ON DELETE CASCADE,
    alert_created_at TEXT    NOT NULL,
    policy_id        INTEGER NOT NULL REFERENCES escalation_policies (id) ON DELETE CASCADE,
    alert_snapshot TEXT    NOT NULL DEFAULT '{}',
    next_step      INTEGER NOT NULL,
    next_due_at    TEXT    NOT NULL,
    completed_at   TEXT,
    updated_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX alert_escalations_due_idx ON alert_escalations (next_due_at)
    WHERE completed_at IS NULL;

ALTER TABLE alerts ADD COLUMN acknowledged_at TEXT;
