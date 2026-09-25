-- Per-channel delivery health. A channel whose credential was revoked (or
-- whose webhook was deleted) used to fail every single alert forever, and the
-- only way to notice was reading delivery_attempts one alert at a time. These
-- counters let a channel say it is broken, and let SoroBeacon take it out of
-- rotation once it is.
--
-- Everything here is derived from delivery outcomes, so it can always be
-- rebuilt: consecutive_failures counts failed deliveries since the last
-- success, and a single success clears it. consecutive_permanent_failures
-- counts only failures the channel will not recover from on its own
-- (401/403/404). It is what auto-disable triggers on, and a transient 5xx or
-- timeout leaves it alone rather than letting provider noise mask a revoked
-- credential behind it. Both are reset by the migration of an operator
-- explicitly re-enabling the channel.
--
-- last_error holds the most recent failure message. The notifiers strip
-- credentials (webhook URLs, bot tokens) before building those messages, so
-- nothing secret is stored here. disabled_at is set only when health tracking
-- turned the channel off, which is what tells the dashboard to say so.
ALTER TABLE channels ADD COLUMN consecutive_failures BIGINT NOT NULL DEFAULT 0;
ALTER TABLE channels ADD COLUMN consecutive_permanent_failures BIGINT NOT NULL DEFAULT 0;
ALTER TABLE channels ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE channels ADD COLUMN last_error_at TIMESTAMPTZ;
ALTER TABLE channels ADD COLUMN last_success_at TIMESTAMPTZ;
ALTER TABLE channels ADD COLUMN disabled_at TIMESTAMPTZ;
