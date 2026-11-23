-- Reverse of 0033_federated_alerts.
--
-- The federated rows cannot be represented by the old schema: an alert with
-- no local monitor and no local rule has nowhere to point. They are deleted
-- rather than kept in a shape the old columns cannot hold.
--
-- SQLite cannot restore a NOT NULL constraint without rebuilding the table,
-- so the rebuild is the mirror of the up migration: the alerts table and the
-- three tables that reference it are recreated with the old shape.

DELETE FROM alerts WHERE monitor_id IS NULL OR rule_id IS NULL;

CREATE TABLE alerts_old (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    rule_id    INTEGER NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    event_id   TEXT    NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '{}',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ledger     INTEGER NOT NULL DEFAULT 0,
    retracted_at TEXT,
    backfilled INTEGER NOT NULL DEFAULT 0,
    severity   TEXT    NOT NULL DEFAULT 'warning',
    inhibited_by_rule_id INTEGER REFERENCES rules (id) ON DELETE SET NULL,
    enrichment TEXT,
    suppressed INTEGER NOT NULL DEFAULT 0,
    suppression_reason TEXT NOT NULL DEFAULT '',
    acknowledged_at TEXT,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    network    TEXT NOT NULL DEFAULT ''
);

INSERT INTO alerts_old (id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at,
                         backfilled, severity, inhibited_by_rule_id, enrichment, suppressed,
                         suppression_reason, acknowledged_at, workspace_id, network)
SELECT id, monitor_id, rule_id, event_id, payload, created_at, ledger, retracted_at,
       backfilled, severity, inhibited_by_rule_id, enrichment, suppressed,
       suppression_reason, acknowledged_at, workspace_id, network
FROM alerts
WHERE monitor_id IS NOT NULL AND rule_id IS NOT NULL;

CREATE TABLE delivery_attempts_old (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    alert_id         INTEGER NOT NULL REFERENCES alerts_old (id) ON DELETE CASCADE,
    channel_id       INTEGER NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    status           TEXT    NOT NULL,
    response_snippet TEXT    NOT NULL DEFAULT '',
    attempted_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO delivery_attempts_old SELECT * FROM delivery_attempts;

CREATE TABLE alert_escalations_old (
    alert_id         INTEGER PRIMARY KEY REFERENCES alerts_old (id) ON DELETE CASCADE,
    alert_created_at TEXT    NOT NULL,
    policy_id        INTEGER NOT NULL REFERENCES escalation_policies (id) ON DELETE CASCADE,
    alert_snapshot   TEXT    NOT NULL DEFAULT '{}',
    next_step        INTEGER NOT NULL,
    next_due_at      TEXT    NOT NULL,
    completed_at     TEXT,
    updated_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO alert_escalations_old SELECT * FROM alert_escalations;

CREATE TABLE dead_letters_old (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    alert_id         INTEGER NOT NULL REFERENCES alerts_old (id) ON DELETE CASCADE,
    alert_created_at TEXT    NOT NULL,
    channel_id       INTEGER NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    last_error       TEXT    NOT NULL DEFAULT '',
    attempt_count    INTEGER NOT NULL,
    last_status      INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO dead_letters_old SELECT * FROM dead_letters;

DROP TABLE delivery_attempts;
DROP TABLE alert_escalations;
DROP TABLE dead_letters;
DROP TABLE alerts;

ALTER TABLE alerts_old RENAME TO alerts;
ALTER TABLE delivery_attempts_old RENAME TO delivery_attempts;
ALTER TABLE alert_escalations_old RENAME TO alert_escalations;
ALTER TABLE dead_letters_old RENAME TO dead_letters;

CREATE UNIQUE INDEX alerts_rule_event_uidx ON alerts (rule_id, event_id);
CREATE INDEX alerts_monitor_created_idx
    ON alerts (monitor_id, created_at DESC);
CREATE INDEX alerts_rule_created_idx
    ON alerts (rule_id, created_at DESC, id DESC);
CREATE INDEX alerts_contract_created_idx
    ON alerts ((json_extract(payload, '$.contract_id')), created_at DESC, id DESC);
CREATE INDEX alerts_monitor_rule_created_idx
    ON alerts (monitor_id, rule_id, created_at DESC, id DESC);
CREATE INDEX alerts_created_at_idx ON alerts (created_at);
CREATE INDEX alerts_ledger_idx ON alerts (ledger);
CREATE INDEX alerts_inhibited_by_idx
    ON alerts (inhibited_by_rule_id) WHERE inhibited_by_rule_id IS NOT NULL;
CREATE INDEX alerts_workspace_created_idx
    ON alerts (workspace_id, created_at DESC, id DESC);
CREATE INDEX alerts_network_created_idx
    ON alerts (network, created_at DESC, id DESC);
CREATE INDEX delivery_attempts_alert_idx ON delivery_attempts (alert_id);
CREATE INDEX delivery_attempts_channel_attempted_idx
    ON delivery_attempts (channel_id, attempted_at);
CREATE INDEX alert_escalations_due_idx
    ON alert_escalations (next_due_at) WHERE completed_at IS NULL;
CREATE INDEX dead_letters_alert_idx ON dead_letters (alert_id);
CREATE INDEX dead_letters_channel_idx ON dead_letters (channel_id);
