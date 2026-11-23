-- Per-monitor delivery budget; see the Postgres migration of the same number
-- for the reasoning. NULL delivery_budget means unlimited. SQLite cannot add a
-- column with a CHECK that references it in the same statement on older
-- versions portably, so positivity is enforced by the store instead.
ALTER TABLE monitors ADD COLUMN delivery_budget INTEGER;
ALTER TABLE monitors ADD COLUMN budget_window_start TEXT;
ALTER TABLE monitors ADD COLUMN budget_used INTEGER NOT NULL DEFAULT 0;
ALTER TABLE monitors ADD COLUMN budget_exhausted_at TEXT;
