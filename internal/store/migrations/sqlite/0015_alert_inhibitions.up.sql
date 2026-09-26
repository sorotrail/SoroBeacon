-- SQLite equivalent of the Postgres 0009_alert_inhibitions. BIGINT becomes
-- INTEGER, TIMESTAMPTZ becomes TEXT in the fixed format. Foreign keys with
-- ON DELETE CASCADE match the Postgres behaviour (see 0001_init).
CREATE TABLE alert_inhibitions (
    source_rule_id        INTEGER NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    target_rule_id        INTEGER NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    firing_window_seconds INTEGER NOT NULL DEFAULT 300,
    created_at            TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (source_rule_id, target_rule_id)
);
CREATE INDEX alert_inhibitions_target_idx ON alert_inhibitions (target_rule_id);

ALTER TABLE alerts ADD COLUMN inhibited_by_rule_id INTEGER REFERENCES rules (id) ON DELETE SET NULL;
CREATE INDEX alerts_inhibited_by_idx ON alerts (inhibited_by_rule_id) WHERE inhibited_by_rule_id IS NOT NULL;
