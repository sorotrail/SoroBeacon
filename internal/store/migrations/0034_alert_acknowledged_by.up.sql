-- Who acknowledged the alert. acknowledged_at (0024) says when; the
-- dashboard's acknowledge action also records the credential that pressed
-- the button — the token's label when the credential carries one, otherwise
-- the request id, which is the same value the audit row records — so the
-- alert page can answer "who stopped the escalation?" without joining the
-- audit log. NOT NULL DEFAULT '' follows suppression_reason (0022): the Go
-- side reads the column into a plain string, and '' pairs with a NULL
-- acknowledged_at to mean "nobody has".
ALTER TABLE alerts ADD COLUMN acknowledged_by TEXT NOT NULL DEFAULT '';
