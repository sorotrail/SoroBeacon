-- SQLite half of 0034_alert_acknowledged_by: who acknowledged the alert.
-- The rationale lives in the Postgres file; the statement is the same
-- ALTER, because SQLite grows a TEXT column the way Postgres does, and the
-- same default keeps '' — never NULL — in the column for every row.
ALTER TABLE alerts ADD COLUMN acknowledged_by TEXT NOT NULL DEFAULT '';
