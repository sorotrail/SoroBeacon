ALTER TABLE alerts DROP COLUMN response_guidance;
ALTER TABLE alerts DROP COLUMN runbook_url;
ALTER TABLE rules DROP COLUMN response_guidance;
ALTER TABLE rules DROP COLUMN runbook_url;
ALTER TABLE monitors DROP COLUMN response_guidance;
ALTER TABLE monitors DROP COLUMN runbook_url;
ALTER TABLE workspaces DROP COLUMN default_response_guidance;
ALTER TABLE workspaces DROP COLUMN default_runbook_url;
