-- Time-bounded maintenance windows that suppress delivery (not detection).
-- An alert raised during a window is still persisted; it is only marked
-- suppressed and not fanned out to channels.
CREATE TABLE maintenance_windows (
    id          BIGSERIAL PRIMARY KEY,
    reason      TEXT        NOT NULL,
    scope       TEXT        NOT NULL CHECK (scope IN ('global', 'monitor', 'contract')),
    monitor_id  BIGINT      REFERENCES monitors (id) ON DELETE CASCADE,
    contract_id TEXT,
    start_at    TIMESTAMPTZ NOT NULL,
    end_at      TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- An open-ended silence is the failure mode this feature exists to
    -- prevent, so a window must end strictly after it starts.
    CHECK (end_at > start_at),
    -- Each scope carries exactly the identifiers it needs and no others.
    CHECK (
        (scope = 'global'   AND monitor_id IS NULL     AND contract_id IS NULL) OR
        (scope = 'monitor'  AND monitor_id IS NOT NULL AND contract_id IS NULL) OR
        (scope = 'contract' AND monitor_id IS NULL     AND contract_id IS NOT NULL)
    )
);

-- The delivery path looks up an active window by time; this index keeps that
-- a single indexed range scan.
CREATE INDEX maintenance_windows_window_idx ON maintenance_windows (start_at, end_at);
CREATE INDEX maintenance_windows_monitor_idx ON maintenance_windows (monitor_id) WHERE monitor_id IS NOT NULL;
CREATE INDEX maintenance_windows_contract_idx ON maintenance_windows (contract_id) WHERE contract_id IS NOT NULL;

-- Suppressed alerts stay visible: the dashboard shows them marked, with the
-- window's reason.
ALTER TABLE alerts ADD COLUMN suppressed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE alerts ADD COLUMN suppression_reason TEXT NOT NULL DEFAULT '';
