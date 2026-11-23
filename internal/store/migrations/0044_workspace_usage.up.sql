-- Durable aggregated usage records per workspace and UTC hour period.
-- Rollups are updated in batches from in-memory counters rather than on every
-- request, and survive process restarts for reporting and invoicing.
CREATE TABLE IF NOT EXISTS workspace_usage_rollups (
    workspace_id     TEXT        NOT NULL,
    period_start     TIMESTAMPTZ NOT NULL,
    period_end       TIMESTAMPTZ NOT NULL,
    alerts_raised    BIGINT      NOT NULL DEFAULT 0,
    rule_evaluations BIGINT      NOT NULL DEFAULT 0,
    api_requests     BIGINT      NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, period_start)
);

CREATE INDEX IF NOT EXISTS idx_workspace_usage_rollups_period
    ON workspace_usage_rollups (period_start, workspace_id);

CREATE TABLE IF NOT EXISTS workspace_notification_usage_rollups (
    workspace_id TEXT        NOT NULL,
    period_start TIMESTAMPTZ NOT NULL,
    period_end   TIMESTAMPTZ NOT NULL,
    channel_type TEXT        NOT NULL,
    outcome      TEXT        NOT NULL,
    count        BIGINT      NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, period_start, channel_type, outcome)
);

CREATE INDEX IF NOT EXISTS idx_workspace_notification_usage_rollups_period
    ON workspace_notification_usage_rollups (period_start, workspace_id);
