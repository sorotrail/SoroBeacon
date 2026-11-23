-- Pre-computed alert statistics rollups per workspace, monitor, rule, and
-- severity across alert lifecycle states (active, acknowledged, suppressed,
-- inhibited, retracted).
CREATE TABLE IF NOT EXISTS alert_stat_rollups (
    workspace_id       TEXT        NOT NULL DEFAULT 'default',
    monitor_id         BIGINT      NOT NULL DEFAULT 0,
    rule_id            BIGINT      NOT NULL DEFAULT 0,
    severity           TEXT        NOT NULL DEFAULT 'info',
    total_count        BIGINT      NOT NULL DEFAULT 0,
    active_count       BIGINT      NOT NULL DEFAULT 0,
    acknowledged_count BIGINT      NOT NULL DEFAULT 0,
    suppressed_count   BIGINT      NOT NULL DEFAULT 0,
    inhibited_count    BIGINT      NOT NULL DEFAULT 0,
    retracted_count    BIGINT      NOT NULL DEFAULT 0,
    last_alert_at      TIMESTAMPTZ,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, monitor_id, rule_id, severity)
);

CREATE INDEX IF NOT EXISTS alert_stat_rollups_workspace_idx ON alert_stat_rollups (workspace_id);
CREATE INDEX IF NOT EXISTS alert_stat_rollups_monitor_idx ON alert_stat_rollups (workspace_id, monitor_id);

CREATE OR REPLACE FUNCTION maintain_alert_stat_rollup() RETURNS TRIGGER AS $$
DECLARE
    old_active BIGINT := 0;
    old_ack    BIGINT := 0;
    old_supp   BIGINT := 0;
    old_inhib  BIGINT := 0;
    old_retr   BIGINT := 0;
    new_active BIGINT := 0;
    new_ack    BIGINT := 0;
    new_supp   BIGINT := 0;
    new_inhib  BIGINT := 0;
    new_retr   BIGINT := 0;
    sev        TEXT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        sev := COALESCE(NULLIF(NEW.severity, ''), 'info');
        IF NEW.retracted_at IS NOT NULL THEN
            new_retr := 1;
        ELSE
            IF NEW.acknowledged_at IS NOT NULL THEN new_ack := 1; END IF;
            IF COALESCE(NEW.suppressed, FALSE) THEN new_supp := 1; END IF;
            IF NEW.inhibited_by_rule_id IS NOT NULL THEN new_inhib := 1; END IF;
            IF NEW.acknowledged_at IS NULL AND NOT COALESCE(NEW.suppressed, FALSE) AND NEW.inhibited_by_rule_id IS NULL THEN
                new_active := 1;
            END IF;
        END IF;

        INSERT INTO alert_stat_rollups (
            workspace_id, monitor_id, rule_id, severity,
            total_count, active_count, acknowledged_count, suppressed_count, inhibited_count, retracted_count,
            last_alert_at, updated_at
        ) VALUES (
            NEW.workspace_id, COALESCE(NEW.monitor_id, 0), COALESCE(NEW.rule_id, 0), sev,
            1, new_active, new_ack, new_supp, new_inhib, new_retr,
            NEW.created_at, now()
        )
        ON CONFLICT (workspace_id, monitor_id, rule_id, severity) DO UPDATE SET
            total_count        = alert_stat_rollups.total_count + 1,
            active_count       = alert_stat_rollups.active_count + EXCLUDED.active_count,
            acknowledged_count = alert_stat_rollups.acknowledged_count + EXCLUDED.acknowledged_count,
            suppressed_count   = alert_stat_rollups.suppressed_count + EXCLUDED.suppressed_count,
            inhibited_count    = alert_stat_rollups.inhibited_count + EXCLUDED.inhibited_count,
            retracted_count    = alert_stat_rollups.retracted_count + EXCLUDED.retracted_count,
            last_alert_at      = GREATEST(alert_stat_rollups.last_alert_at, EXCLUDED.last_alert_at),
            updated_at         = now();
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        sev := COALESCE(NULLIF(NEW.severity, ''), 'info');
        IF OLD.retracted_at IS NOT NULL THEN
            old_retr := 1;
        ELSE
            IF OLD.acknowledged_at IS NOT NULL THEN old_ack := 1; END IF;
            IF COALESCE(OLD.suppressed, FALSE) THEN old_supp := 1; END IF;
            IF OLD.inhibited_by_rule_id IS NOT NULL THEN old_inhib := 1; END IF;
            IF OLD.acknowledged_at IS NULL AND NOT COALESCE(OLD.suppressed, FALSE) AND OLD.inhibited_by_rule_id IS NULL THEN
                old_active := 1;
            END IF;
        END IF;

        IF NEW.retracted_at IS NOT NULL THEN
            new_retr := 1;
        ELSE
            IF NEW.acknowledged_at IS NOT NULL THEN new_ack := 1; END IF;
            IF COALESCE(NEW.suppressed, FALSE) THEN new_supp := 1; END IF;
            IF NEW.inhibited_by_rule_id IS NOT NULL THEN new_inhib := 1; END IF;
            IF NEW.acknowledged_at IS NULL AND NOT COALESCE(NEW.suppressed, FALSE) AND NEW.inhibited_by_rule_id IS NULL THEN
                new_active := 1;
            END IF;
        END IF;

        UPDATE alert_stat_rollups SET
            active_count       = active_count - old_active + new_active,
            acknowledged_count = acknowledged_count - old_ack + new_ack,
            suppressed_count   = suppressed_count - old_supp + new_supp,
            inhibited_count    = inhibited_count - old_inhib + new_inhib,
            retracted_count    = retracted_count - old_retr + new_retr,
            updated_at         = now()
        WHERE workspace_id = NEW.workspace_id
          AND monitor_id = COALESCE(NEW.monitor_id, 0)
          AND rule_id = COALESCE(NEW.rule_id, 0)
          AND severity = sev;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        sev := COALESCE(NULLIF(OLD.severity, ''), 'info');
        IF OLD.retracted_at IS NOT NULL THEN
            old_retr := 1;
        ELSE
            IF OLD.acknowledged_at IS NOT NULL THEN old_ack := 1; END IF;
            IF COALESCE(OLD.suppressed, FALSE) THEN old_supp := 1; END IF;
            IF OLD.inhibited_by_rule_id IS NOT NULL THEN old_inhib := 1; END IF;
            IF OLD.acknowledged_at IS NULL AND NOT COALESCE(OLD.suppressed, FALSE) AND OLD.inhibited_by_rule_id IS NULL THEN
                old_active := 1;
            END IF;
        END IF;

        UPDATE alert_stat_rollups SET
            total_count        = GREATEST(0, total_count - 1),
            active_count       = GREATEST(0, active_count - old_active),
            acknowledged_count = GREATEST(0, acknowledged_count - old_ack),
            suppressed_count   = GREATEST(0, suppressed_count - old_supp),
            inhibited_count    = GREATEST(0, inhibited_count - old_inhib),
            retracted_count    = GREATEST(0, retracted_count - old_retr),
            updated_at         = now()
        WHERE workspace_id = OLD.workspace_id
          AND monitor_id = COALESCE(OLD.monitor_id, 0)
          AND rule_id = COALESCE(OLD.rule_id, 0)
          AND severity = sev;
        RETURN OLD;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_alerts_rollup ON alerts;
CREATE TRIGGER trg_alerts_rollup
AFTER INSERT OR UPDATE OR DELETE ON alerts
FOR EACH ROW EXECUTE FUNCTION maintain_alert_stat_rollup();

INSERT INTO alert_stat_rollups (
    workspace_id, monitor_id, rule_id, severity,
    total_count, active_count, acknowledged_count, suppressed_count, inhibited_count, retracted_count,
    last_alert_at, updated_at
)
SELECT
    workspace_id,
    COALESCE(monitor_id, 0),
    COALESCE(rule_id, 0),
    COALESCE(NULLIF(severity, ''), 'info'),
    COUNT(*),
    COUNT(*) FILTER (WHERE retracted_at IS NULL AND acknowledged_at IS NULL AND NOT COALESCE(suppressed, FALSE) AND inhibited_by_rule_id IS NULL),
    COUNT(*) FILTER (WHERE retracted_at IS NULL AND acknowledged_at IS NOT NULL),
    COUNT(*) FILTER (WHERE retracted_at IS NULL AND COALESCE(suppressed, FALSE)),
    COUNT(*) FILTER (WHERE retracted_at IS NULL AND inhibited_by_rule_id IS NOT NULL),
    COUNT(*) FILTER (WHERE retracted_at IS NOT NULL),
    MAX(created_at),
    now()
FROM alerts
GROUP BY workspace_id, COALESCE(monitor_id, 0), COALESCE(rule_id, 0), COALESCE(NULLIF(severity, ''), 'info')
ON CONFLICT (workspace_id, monitor_id, rule_id, severity) DO UPDATE SET
    total_count        = EXCLUDED.total_count,
    active_count       = EXCLUDED.active_count,
    acknowledged_count = EXCLUDED.acknowledged_count,
    suppressed_count   = EXCLUDED.suppressed_count,
    inhibited_count    = EXCLUDED.inhibited_count,
    retracted_count    = EXCLUDED.retracted_count,
    last_alert_at      = EXCLUDED.last_alert_at,
    updated_at         = now();
