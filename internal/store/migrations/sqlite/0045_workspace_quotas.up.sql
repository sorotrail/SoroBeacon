-- SQLite half of 0045_workspace_quotas: per-workspace resource quotas and
-- shared REST/GraphQL query cost budget state.
CREATE TABLE IF NOT EXISTS workspace_quotas (
    workspace_id              TEXT    PRIMARY KEY,
    max_monitors              INTEGER NOT NULL DEFAULT 0 CHECK (max_monitors >= 0),
    max_rules                 INTEGER NOT NULL DEFAULT 0 CHECK (max_rules >= 0),
    max_channels              INTEGER NOT NULL DEFAULT 0 CHECK (max_channels >= 0),
    max_subscriptions         INTEGER NOT NULL DEFAULT 0 CHECK (max_subscriptions >= 0),
    query_cost_budget         REAL    NOT NULL DEFAULT 0 CHECK (query_cost_budget >= 0),
    query_cost_refill_per_sec REAL    NOT NULL DEFAULT 0 CHECK (query_cost_refill_per_sec >= 0),
    updated_at                TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS workspace_query_budget_state (
    workspace_id TEXT PRIMARY KEY,
    tokens       REAL NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
