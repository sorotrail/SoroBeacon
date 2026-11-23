-- SQLite half of 0047_workspace_deletions: durable progress and completion
-- record for bounded, resumable workspace deletion.
CREATE TABLE IF NOT EXISTS workspace_deletions (
    workspace_id              TEXT    PRIMARY KEY,
    status                    TEXT    NOT NULL DEFAULT 'stopping' CHECK (status IN ('stopping', 'deleting_alerts', 'deleting_resources', 'completed')),
    actor                     TEXT    NOT NULL DEFAULT '',
    batch_size                INTEGER NOT NULL DEFAULT 500 CHECK (batch_size > 0),
    batches_completed         INTEGER NOT NULL DEFAULT 0,
    last_alert_id             INTEGER NOT NULL DEFAULT 0,
    alerts_deleted            INTEGER NOT NULL DEFAULT 0,
    delivery_attempts_deleted INTEGER NOT NULL DEFAULT 0,
    dead_letters_deleted      INTEGER NOT NULL DEFAULT 0,
    alert_escalations_deleted INTEGER NOT NULL DEFAULT 0,
    alert_dedup_deleted       INTEGER NOT NULL DEFAULT 0,
    monitors_deleted          INTEGER NOT NULL DEFAULT 0,
    rules_deleted             INTEGER NOT NULL DEFAULT 0,
    channels_deleted          INTEGER NOT NULL DEFAULT 0,
    saved_searches_deleted    INTEGER NOT NULL DEFAULT 0,
    monitor_templates_deleted INTEGER NOT NULL DEFAULT 0,
    api_tokens_deleted        INTEGER NOT NULL DEFAULT 0,
    counts_json               TEXT    NOT NULL DEFAULT '{}',
    started_at                TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at                TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    completed_at              TEXT
);

CREATE INDEX IF NOT EXISTS workspace_deletions_status_idx
    ON workspace_deletions (status, updated_at DESC);
