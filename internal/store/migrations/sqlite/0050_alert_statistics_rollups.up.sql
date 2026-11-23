-- Pre-computed alert statistics rollups per workspace, monitor, rule, and
-- severity across alert lifecycle states.
CREATE TABLE IF NOT EXISTS alert_stat_rollups (
    workspace_id       TEXT    NOT NULL DEFAULT 'default',
    monitor_id         INTEGER NOT NULL DEFAULT 0,
    rule_id            INTEGER NOT NULL DEFAULT 0,
    severity           TEXT    NOT NULL DEFAULT 'info',
    total_count        INTEGER NOT NULL DEFAULT 0,
    active_count       INTEGER NOT NULL DEFAULT 0,
    acknowledged_count INTEGER NOT NULL DEFAULT 0,
    suppressed_count   INTEGER NOT NULL DEFAULT 0,
    inhibited_count    INTEGER NOT NULL DEFAULT 0,
    retracted_count    INTEGER NOT NULL DEFAULT 0,
    last_alert_at      TEXT,
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (workspace_id, monitor_id, rule_id, severity)
);

CREATE INDEX IF NOT EXISTS alert_stat_rollups_workspace_idx ON alert_stat_rollups (workspace_id);
CREATE INDEX IF NOT EXISTS alert_stat_rollups_monitor_idx ON alert_stat_rollups (workspace_id, monitor_id);

CREATE TRIGGER IF NOT EXISTS trg_alerts_rollup_insert
AFTER INSERT ON alerts
BEGIN
    INSERT INTO alert_stat_rollups (
        workspace_id, monitor_id, rule_id, severity,
        total_count, active_count, acknowledged_count, suppressed_count, inhibited_count, retracted_count,
        last_alert_at, updated_at
    ) VALUES (
        NEW.workspace_id,
        COALESCE(NEW.monitor_id, 0),
        COALESCE(NEW.rule_id, 0),
        CASE WHEN NEW.severity IS NULL OR NEW.severity = '' THEN 'info' ELSE NEW.severity END,
        1,
        CASE WHEN NEW.retracted_at IS NULL AND NEW.acknowledged_at IS NULL AND COALESCE(NEW.suppressed, 0) = 0 AND NEW.inhibited_by_rule_id IS NULL THEN 1 ELSE 0 END,
        CASE WHEN NEW.retracted_at IS NULL AND NEW.acknowledged_at IS NOT NULL THEN 1 ELSE 0 END,
        CASE WHEN NEW.retracted_at IS NULL AND COALESCE(NEW.suppressed, 0) != 0 THEN 1 ELSE 0 END,
        CASE WHEN NEW.retracted_at IS NULL AND NEW.inhibited_by_rule_id IS NOT NULL THEN 1 ELSE 0 END,
        CASE WHEN NEW.retracted_at IS NOT NULL THEN 1 ELSE 0 END,
        NEW.created_at,
        strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    )
    ON CONFLICT (workspace_id, monitor_id, rule_id, severity) DO UPDATE SET
        total_count        = alert_stat_rollups.total_count + excluded.total_count,
        active_count       = alert_stat_rollups.active_count + excluded.active_count,
        acknowledged_count = alert_stat_rollups.acknowledged_count + excluded.acknowledged_count,
        suppressed_count   = alert_stat_rollups.suppressed_count + excluded.suppressed_count,
        inhibited_count    = alert_stat_rollups.inhibited_count + excluded.inhibited_count,
        retracted_count    = alert_stat_rollups.retracted_count + excluded.retracted_count,
        last_alert_at      = CASE WHEN alert_stat_rollups.last_alert_at IS NULL OR alert_stat_rollups.last_alert_at < excluded.last_alert_at THEN excluded.last_alert_at ELSE alert_stat_rollups.last_alert_at END,
        updated_at         = excluded.updated_at;
END;

CREATE TRIGGER IF NOT EXISTS trg_alerts_rollup_update
AFTER UPDATE ON alerts
BEGIN
    UPDATE alert_stat_rollups SET
        active_count       = active_count - (CASE WHEN OLD.retracted_at IS NULL AND OLD.acknowledged_at IS NULL AND COALESCE(OLD.suppressed, 0) = 0 AND OLD.inhibited_by_rule_id IS NULL THEN 1 ELSE 0 END)
                                          + (CASE WHEN NEW.retracted_at IS NULL AND NEW.acknowledged_at IS NULL AND COALESCE(NEW.suppressed, 0) = 0 AND NEW.inhibited_by_rule_id IS NULL THEN 1 ELSE 0 END),
        acknowledged_count = acknowledged_count - (CASE WHEN OLD.retracted_at IS NULL AND OLD.acknowledged_at IS NOT NULL THEN 1 ELSE 0 END)
                                                + (CASE WHEN NEW.retracted_at IS NULL AND NEW.acknowledged_at IS NOT NULL THEN 1 ELSE 0 END),
        suppressed_count   = suppressed_count - (CASE WHEN OLD.retracted_at IS NULL AND COALESCE(OLD.suppressed, 0) != 0 THEN 1 ELSE 0 END)
                                              + (CASE WHEN NEW.retracted_at IS NULL AND COALESCE(NEW.suppressed, 0) != 0 THEN 1 ELSE 0 END),
        inhibited_count    = inhibited_count - (CASE WHEN OLD.retracted_at IS NULL AND OLD.inhibited_by_rule_id IS NOT NULL THEN 1 ELSE 0 END)
                                             + (CASE WHEN NEW.retracted_at IS NULL AND NEW.inhibited_by_rule_id IS NOT NULL THEN 1 ELSE 0 END),
        retracted_count    = retracted_count - (CASE WHEN OLD.retracted_at IS NOT NULL THEN 1 ELSE 0 END)
                                             + (CASE WHEN NEW.retracted_at IS NOT NULL THEN 1 ELSE 0 END),
        updated_at         = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE workspace_id = NEW.workspace_id
      AND monitor_id = COALESCE(NEW.monitor_id, 0)
      AND rule_id = COALESCE(NEW.rule_id, 0)
      AND severity = (CASE WHEN NEW.severity IS NULL OR NEW.severity = '' THEN 'info' ELSE NEW.severity END);
END;

CREATE TRIGGER IF NOT EXISTS trg_alerts_rollup_delete
AFTER DELETE ON alerts
BEGIN
    UPDATE alert_stat_rollups SET
        total_count        = MAX(0, total_count - 1),
        active_count       = MAX(0, active_count - (CASE WHEN OLD.retracted_at IS NULL AND OLD.acknowledged_at IS NULL AND COALESCE(OLD.suppressed, 0) = 0 AND OLD.inhibited_by_rule_id IS NULL THEN 1 ELSE 0 END)),
        acknowledged_count = MAX(0, acknowledged_count - (CASE WHEN OLD.retracted_at IS NULL AND OLD.acknowledged_at IS NOT NULL THEN 1 ELSE 0 END)),
        suppressed_count   = MAX(0, suppressed_count - (CASE WHEN OLD.retracted_at IS NULL AND COALESCE(OLD.suppressed, 0) != 0 THEN 1 ELSE 0 END)),
        inhibited_count    = MAX(0, inhibited_count - (CASE WHEN OLD.retracted_at IS NULL AND OLD.inhibited_by_rule_id IS NOT NULL THEN 1 ELSE 0 END)),
        retracted_count    = MAX(0, retracted_count - (CASE WHEN OLD.retracted_at IS NOT NULL THEN 1 ELSE 0 END)),
        updated_at         = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE workspace_id = OLD.workspace_id
      AND monitor_id = COALESCE(OLD.monitor_id, 0)
      AND rule_id = COALESCE(OLD.rule_id, 0)
      AND severity = (CASE WHEN OLD.severity IS NULL OR OLD.severity = '' THEN 'info' ELSE OLD.severity END);
END;

INSERT OR REPLACE INTO alert_stat_rollups (
    workspace_id, monitor_id, rule_id, severity,
    total_count, active_count, acknowledged_count, suppressed_count, inhibited_count, retracted_count,
    last_alert_at, updated_at
)
SELECT
    workspace_id,
    COALESCE(monitor_id, 0),
    COALESCE(rule_id, 0),
    CASE WHEN severity IS NULL OR severity = '' THEN 'info' ELSE severity END,
    COUNT(*),
    SUM(CASE WHEN retracted_at IS NULL AND acknowledged_at IS NULL AND COALESCE(suppressed, 0) = 0 AND inhibited_by_rule_id IS NULL THEN 1 ELSE 0 END),
    SUM(CASE WHEN retracted_at IS NULL AND acknowledged_at IS NOT NULL THEN 1 ELSE 0 END),
    SUM(CASE WHEN retracted_at IS NULL AND COALESCE(suppressed, 0) != 0 THEN 1 ELSE 0 END),
    SUM(CASE WHEN retracted_at IS NULL AND inhibited_by_rule_id IS NOT NULL THEN 1 ELSE 0 END),
    SUM(CASE WHEN retracted_at IS NOT NULL THEN 1 ELSE 0 END),
    MAX(created_at),
    strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
FROM alerts
GROUP BY workspace_id, COALESCE(monitor_id, 0), COALESCE(rule_id, 0), CASE WHEN severity IS NULL OR severity = '' THEN 'info' ELSE severity END;
