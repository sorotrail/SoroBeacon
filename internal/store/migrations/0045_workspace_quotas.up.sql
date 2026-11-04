-- Per-workspace resource quotas and shared REST/GraphQL query cost budget state.
-- Zero values mean the quota or budget is unlimited (disabled) for that workspace.
CREATE TABLE IF NOT EXISTS workspace_quotas (
    workspace_id              TEXT             PRIMARY KEY,
    max_monitors              BIGINT           NOT NULL DEFAULT 0 CHECK (max_monitors >= 0),
    max_rules                 BIGINT           NOT NULL DEFAULT 0 CHECK (max_rules >= 0),
    max_channels              BIGINT           NOT NULL DEFAULT 0 CHECK (max_channels >= 0),
    max_subscriptions         BIGINT           NOT NULL DEFAULT 0 CHECK (max_subscriptions >= 0),
    query_cost_budget         DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (query_cost_budget >= 0),
    query_cost_refill_per_sec DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (query_cost_refill_per_sec >= 0),
    updated_at                TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workspace_query_budget_state (
    workspace_id TEXT             PRIMARY KEY,
    tokens       DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ      NOT NULL DEFAULT now()
);
