# Rule Evaluation Isolation and Quarantine

SoroBeacon isolates every rule evaluation inside a bounded execution context so a slow, panicking, or misconfigured rule cannot stall contract cursor advancement or starve other rules and workspaces.

## Isolation Boundary

Every top-level call to `Registry.Evaluate` runs inside an isolation boundary (`internal/rules/isolation.go`) that enforces:

1. **Per-rule evaluation timeout (`DefaultEvalTimeout = 100ms`):**
   - Each rule evaluation runs with a bounded evaluation budget (configurable via `Registry.WithEvalTimeout` and `Poller.WithRuleTimeout`).
   - Time spent in database/storage I/O (tracked via `rules.EnterDB(ctx)` and `rules.RecordDBDuration(ctx, ...)`, such as `frequency_threshold` rebuilding its rolling window from `MatchLog`) is subtracted from wall-clock elapsed time before comparing against the rule's timeout budget. Slow database queries never cause a rule timeout or false quarantine.
2. **Per-rule panic recovery:**
   - Any panic inside a rule evaluator (including inside a `composite` child rule) is recovered at the rule boundary, converted into `rules.ErrRulePanic`, logged at `ERROR` level with `rule_id`, `monitor_id`, `workspace_id`, and `event_id`, and counted in both `sorobeacon_rule_evaluation_panics_total` and `sorobeacon_poll_panics_total`.
3. **Transactional state rollback (`TransactionalRuleEvaluator`):**
   - Stateful evaluators (`frequency_threshold`) implement `SnapshotRule` and `RestoreRule` plus copy-on-write state transitions.
   - If a rule or a `composite` rule panics, times out, or errors partway through evaluation, all stateful child rule mutations for that `rule_id` are rolled back atomically so failed evaluations never leave phantom rolling-window counts or armed episodes behind.
   - `frequency_threshold` also bounds per-rule rolling-window timestamps (`MaxFrequencyMatchesPerRule = 10,000`) and tracked rule states (`MaxTrackedStatefulRules = 10,000`).
4. **Cursor progress preservation:**
   - Rule-attributed failures (`rules.IsRuleIsolatedFailure(err) == true`: timeouts, panics, runtime evaluation errors, and quarantined skips) are isolated to the offending rule and do not hold the contract's durable cursor back.
   - Transient storage failures (`rules.IsStorageError(err) == true`) continue to hold the contract cursor before the failed event so the event is replayed once storage recovers without quarantining the rule.

## Automatic Quarantine and Recovery

When a rule accumulates `DefaultQuarantineThreshold = 3` consecutive rule-attributed failures (configurable via `Registry.WithQuarantineThreshold` and `Poller.WithRuleQuarantineThreshold`):

- The rule transitions to **quarantined** (`rules.ErrRuleQuarantined`).
- Subsequent events skip the quarantined rule immediately without executing its evaluator.
- The transition is logged at `ERROR` level (`"rule quarantined after repeated evaluation failures"`), recorded in the durable audit log (`actor = "system:rule_quarantine"`, `target_type = "rule"`), and reflected in Prometheus metrics.
- **Recovery:**
  - Editing the rule via the API or store increments its `version`, which automatically clears the quarantine and resets its stateful window on the next evaluation.
  - Calling `Poller.ReenableRule(ctx, ruleID)`, `Ingestor.ReenableRule(ctx, ruleID)`, or `Registry.ResetRuleState(ruleID)` clears the quarantine, resets the rule's stateful evaluator state, decrements the quarantine gauge, and writes a recovery entry to the audit log.

## Multi-Workspace Fairness

When multiple monitors across different workspaces watch the same contract:

- Monitors are interleaved round-robin across workspaces (`orderMonitorsByWorkspaceFairness`) while preserving each workspace's relative monitor order.
- Each workspace is bounded by a per-event evaluation time budget (`DefaultWorkspaceEvalBudget = 500ms`, configurable via `Poller.WithWorkspaceEvalBudget`). If a workspace exhausts its per-event budget, its remaining rules on that event are skipped and counted in `sorobeacon_workspace_rule_budget_exhausted_total` while other workspaces continue evaluating normally.

## Prometheus Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `sorobeacon_rule_evaluation_duration_seconds` | Histogram | `network`, `rule_id`, `rule_type` | Pure rule evaluation duration in seconds (excluding attributed database I/O). |
| `sorobeacon_rule_evaluation_failures_total` | Counter | `network`, `rule_id`, `rule_type`, `reason` | Per-rule evaluation failures (`timeout`, `panic`, `error`, `quarantined`). |
| `sorobeacon_rule_evaluation_panics_total` | Counter | `network`, `rule_id`, `rule_type` | Panics recovered during individual rule evaluations. |
| `sorobeacon_rules_quarantined` | Gauge | `network` | Current number of quarantined rules on the network. |
| `sorobeacon_rule_quarantined` | Gauge | `network`, `rule_id`, `rule_type` | `1` while `rule_id` is quarantined, `0` when healthy or re-enabled. |
| `sorobeacon_rule_quarantine_transitions_total` | Counter | `network`, `rule_type`, `reason` | Total transitions into quarantine by rule type and reason. |
| `sorobeacon_workspace_rule_budget_exhausted_total` | Counter | `network`, `workspace` | Rule evaluations skipped because a workspace exhausted its per-event evaluation budget. |
