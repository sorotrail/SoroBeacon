-- SQLite half of 0044_workspace_usage: durable aggregated usage records per
-- workspace and UTC hour period, plus notification breakdown by channel_type
-- and outcome.
CREATE TABLE IF NOT EXISTS workspace_usage_rollups (
    workspace_id     TEXT    NOT NULL,
    period_start     TEXT    NOT NULL,
    period_end       TEXT    NOT NULL,
    alerts_raised    INTEGER NOT NULL DEFAULT 0,
    rule_evaluations INTEGER NOT NULL DEFAULT 0,
    api_requests     INTEGER NOT NULL DEFAULT 0,
    updated_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (workspace_id, period_start)
);

CREATE INDEX IF NOT EXISTS idx_workspace_usage_rollups_period
    ON workspace_usage_rollups (period_start, workspace_id);

CREATE TABLE IF NOT EXISTS workspace_notification_usage_rollups (
    workspace_id TEXT    NOT NULL,
    period_start TEXT    NOT NULL,
    period_end   TEXT    NOT NULL,
    channel_type TEXT    NOT NULL,
    outcome      TEXT    NOT NULL,
    count        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (workspace_id, period_start, channel_type, outcome)
);

CREATE INDEX IF NOT EXISTS idx_workspace_notification_usage_rollups_period
    ON workspace_notification_usage_rollups (period_start, workspace_id);
