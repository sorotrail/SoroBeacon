-- Durable progress and completion record for bounded, resumable workspace deletion.
--
-- Design decisions:
--   * Resumable by construction: deleting a workspace with millions of alerts
--     cannot run as a single unbounded transaction without holding locks across
--     alert partitions and starving live ingestion. Progress is checkpointed here
--     after each bounded alert batch so an interrupted deletion resumes from the
--     remaining alerts and re-running a completed deletion is idempotent.
--   * No foreign key to workspaces(id): the final phase deletes the workspaces
--     row itself while keeping this tombstone so subsequent retries or status
--     checks see that deletion completed cleanly.
CREATE TABLE IF NOT EXISTS workspace_deletions (
    workspace_id              TEXT        PRIMARY KEY,
    status                    TEXT        NOT NULL DEFAULT 'stopping' CHECK (status IN ('stopping', 'deleting_alerts', 'deleting_resources', 'completed')),
    actor                     TEXT        NOT NULL DEFAULT '',
    batch_size                INTEGER     NOT NULL DEFAULT 500 CHECK (batch_size > 0),
    batches_completed         BIGINT      NOT NULL DEFAULT 0,
    last_alert_id             BIGINT      NOT NULL DEFAULT 0,
    alerts_deleted            BIGINT      NOT NULL DEFAULT 0,
    delivery_attempts_deleted BIGINT      NOT NULL DEFAULT 0,
    dead_letters_deleted      BIGINT      NOT NULL DEFAULT 0,
    alert_escalations_deleted BIGINT      NOT NULL DEFAULT 0,
    alert_dedup_deleted       BIGINT      NOT NULL DEFAULT 0,
    monitors_deleted          BIGINT      NOT NULL DEFAULT 0,
    rules_deleted             BIGINT      NOT NULL DEFAULT 0,
    channels_deleted          BIGINT      NOT NULL DEFAULT 0,
    saved_searches_deleted    BIGINT      NOT NULL DEFAULT 0,
    monitor_templates_deleted BIGINT      NOT NULL DEFAULT 0,
    api_tokens_deleted        BIGINT      NOT NULL DEFAULT 0,
    counts_json               TEXT        NOT NULL DEFAULT '{}',
    started_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at              TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS workspace_deletions_status_idx
    ON workspace_deletions (status, updated_at DESC);
