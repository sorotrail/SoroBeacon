-- Filter combinations for GET /alerts: rule, contract (from payload), and
-- monitor+rule, all ordered the same way the listing pages (created_at, id).
-- SQLite has no JSONB accessor operator, so the contract index is an
-- expression index over json_extract(payload, '$.contract_id'), which is the
-- exact expression the queries use. DESC indexes are scanned backwards for
-- created_at_asc.
CREATE INDEX alerts_rule_created_idx
    ON alerts (rule_id, created_at DESC, id DESC);

CREATE INDEX alerts_contract_created_idx
    ON alerts ((json_extract(payload, '$.contract_id')), created_at DESC, id DESC);

CREATE INDEX alerts_monitor_rule_created_idx
    ON alerts (monitor_id, rule_id, created_at DESC, id DESC);
