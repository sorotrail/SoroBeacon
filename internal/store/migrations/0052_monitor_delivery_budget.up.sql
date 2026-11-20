-- Per-monitor delivery budget. A misconfigured rule on a busy contract can
-- deliver thousands of notifications in minutes; channel rate limiting only
-- paces that, so this is the backstop that stops it.
--
-- The budget lives on the monitor row rather than in a table of its own: it is
-- one small record per monitor, it must be read and updated atomically with
-- the decision it drives, and it should disappear with the monitor.
--
-- delivery_budget NULL means unlimited, which is the default, so existing
-- monitors behave exactly as before. The counter is durable on purpose: a
-- restart must not reopen the floodgate. budget_window_start is the UTC day
-- the counter belongs to; budget_exhausted_at is set the moment the budget is
-- spent and is what makes the exhaustion notice go out exactly once.
ALTER TABLE monitors
    ADD COLUMN delivery_budget      INTEGER CHECK (delivery_budget IS NULL OR delivery_budget > 0),
    ADD COLUMN budget_window_start  TIMESTAMPTZ,
    ADD COLUMN budget_used          INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN budget_exhausted_at  TIMESTAMPTZ;
