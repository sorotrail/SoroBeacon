-- SQLite half of 0043_runbook_guidance: attach optional runbook links and
-- response guidance to workspaces, monitors, rules, and alerts.
ALTER TABLE workspaces ADD COLUMN default_runbook_url TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN default_response_guidance TEXT NOT NULL DEFAULT '';
ALTER TABLE monitors ADD COLUMN runbook_url TEXT NOT NULL DEFAULT '';
ALTER TABLE monitors ADD COLUMN response_guidance TEXT NOT NULL DEFAULT '';
ALTER TABLE rules ADD COLUMN runbook_url TEXT NOT NULL DEFAULT '';
ALTER TABLE rules ADD COLUMN response_guidance TEXT NOT NULL DEFAULT '';
ALTER TABLE alerts ADD COLUMN runbook_url TEXT NOT NULL DEFAULT '';
ALTER TABLE alerts ADD COLUMN response_guidance TEXT NOT NULL DEFAULT '';
