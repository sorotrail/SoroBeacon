-- SQLite equivalent of the Postgres 0022_maintenance_windows. BIGSERIAL
-- becomes INTEGER PRIMARY KEY AUTOINCREMENT, TIMESTAMPTZ becomes TEXT in the
-- fixed '%Y-%m-%dT%H:%M:%fZ' format (so [start_at, end_at) compares
-- chronologically as text), and BOOLEAN becomes INTEGER 0/1. The CHECK
-- constraints carry over unchanged: a window must be bounded and each scope
-- must carry exactly the identifiers it needs and no others.
CREATE TABLE maintenance_windows (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    reason      TEXT    NOT NULL,
    scope       TEXT    NOT NULL CHECK (scope IN ('global', 'monitor', 'contract')),
    monitor_id  INTEGER REFERENCES monitors (id) ON DELETE CASCADE,
    contract_id TEXT,
    start_at    TEXT    NOT NULL,
    end_at      TEXT    NOT NULL,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
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
ALTER TABLE alerts ADD COLUMN suppressed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE alerts ADD COLUMN suppression_reason TEXT NOT NULL DEFAULT '';
