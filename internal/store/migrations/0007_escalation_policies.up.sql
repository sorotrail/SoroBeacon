-- Escalation policy attached to a monitor: an ordered list of steps, each with
-- a delay and a set of channels. A monitor with no policy keeps the flat
-- fan-out it has always had, so this is purely additive.
CREATE TABLE escalation_policies (
    id         BIGSERIAL   PRIMARY KEY,
    monitor_id BIGINT      NOT NULL UNIQUE REFERENCES monitors (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE escalation_steps (
    id            BIGSERIAL PRIMARY KEY,
    policy_id     BIGINT    NOT NULL REFERENCES escalation_policies (id) ON DELETE CASCADE,
    position      INTEGER   NOT NULL,
    delay_seconds BIGINT    NOT NULL DEFAULT 0,
    UNIQUE (policy_id, position)
);

-- ON DELETE RESTRICT is deliberate: a channel referenced by a policy step must
-- not disappear silently and leave the escalation with nowhere to go. The API
-- reports a 409 before the FK fires, and the constraint is the backstop.
CREATE TABLE escalation_step_channels (
    step_id    BIGINT NOT NULL REFERENCES escalation_steps (id) ON DELETE CASCADE,
    channel_id BIGINT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    PRIMARY KEY (step_id, channel_id)
);

-- Scheduling state for one escalating alert. next_due_at is persisted (not a
-- time.Timer) so an escalation that is mid-flight when the process restarts
-- resumes from its next-due step instead of restarting or being dropped. The
-- alert snapshot lets a later step be delivered without rebuilding the
-- notification payload from scratch.
CREATE TABLE alert_escalations (
    alert_id       BIGINT      PRIMARY KEY REFERENCES alerts (id) ON DELETE CASCADE,
    policy_id      BIGINT      NOT NULL REFERENCES escalation_policies (id) ON DELETE CASCADE,
    alert_snapshot JSONB       NOT NULL DEFAULT '{}',
    next_step      INTEGER     NOT NULL,
    next_due_at    TIMESTAMPTZ NOT NULL,
    completed_at   TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The scheduler scans for due, unfinished escalations; a partial index keeps
-- that scan off completed rows.
CREATE INDEX alert_escalations_due_idx ON alert_escalations (next_due_at)
    WHERE completed_at IS NULL;

-- Set when an operator acknowledges the alert. An acknowledged alert stops
-- escalating; the column lives here so the acknowledgement workflow issue can
-- build on it without another migration.
ALTER TABLE alerts ADD COLUMN acknowledged_at TIMESTAMPTZ;
