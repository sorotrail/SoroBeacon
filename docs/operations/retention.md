# Alert retention and partitioning

> **Warning: retention permanently deletes alert history.** A whole expired
> month is removed with Postgres `DROP TABLE`, and the ragged edge is removed
> with ordinary `DELETE`s. Neither is a soft delete. There is no undo, recycle
> bin, confirmation prompt, or dry-run mode. `sorobeacon backup` stores
> configuration only and never alert rows. If you need the data, configure
> `ARCHIVE_URL` or take a `pg_dump` before shortening the window.

Alert retention is off by default: unset `ALERT_RETENTION` keeps history
forever. Once it is set, the first prune pass runs as soon as the elected
leader starts, then once an hour.

## How retention runs

On Postgres the retention pass is:

1. Make sure the upcoming monthly partitions exist (see the schedule below).
2. Unless `ARCHIVE_URL` is set, drop every monthly partition whose entire
   range is older than the cut-off `now - ALERT_RETENTION`. This is the cheap
   part: a partition drop is DDL.
3. Delete the remaining alerts older than the cut-off in batches of 1000,
   oldest first, until a batch comes back short. Without an archive this is the
   ragged edge: rows in the current month and in the month containing the
   cut-off. With an archive, step 2 is skipped and this covers every expired
   row.
4. Log one `alert retention prune` line with `deleted`, `cutoff`, and
   `archiving`.

The drop path explicitly clears `delivery_attempts` and `alert_dedup` for the
partition before detaching and dropping it. A `DROP` does not fire
`ON DELETE CASCADE`, so those children have to be removed first. Row deletes do
use the schema's cascades, including the cascade to `delivery_attempts`.

SQLite has no partitions. It uses only the batched row-delete path; the
partition details below apply to Postgres.

The interval and batch size are constants in `internal/store/prune.go`:
`DefaultPruneInterval` is one hour and `DefaultPruneBatch` is 1000. Neither is
configurable in this release.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `ALERT_RETENTION` | unset (keep forever) | Positive duration (`24h`, `2160h`) or a day value (`90d`, `1.5d`). The cut-off is `now - ALERT_RETENTION`. A zero or negative value is rejected at startup rather than silently keeping or deleting everything. |
| `ARCHIVE_URL` | unset (off) | Copy each expired batch before deleting it. A local directory, `file://`, `dir://`, or `s3://bucket/prefix`. Archives require `ALERT_RETENTION`; setting only `ARCHIVE_URL` logs `ARCHIVE_URL is set but ALERT_RETENTION is unset; nothing will be archived or deleted`. |

See the [environment variable reference](../configuration.md) for the archive
URL's region and credential options. Archive objects contain alert rows only;
channel `config` (webhook URLs, bot tokens, SMTP credentials) is never read by
the archiver.

## Partition creation schedule

`alerts` is range-partitioned by `created_at` into one child per UTC calendar
month, named `alerts_YYYY_MM`, plus `alerts_default`.

| When | Window created |
| --- | --- |
| Migration `0011_alerts_partitioning` | Every month containing existing alerts, then the current month through twelve months ahead. |
| Every process startup | The current month plus the next two months (`EnsureAlertPartitions(now, 3)`). A failure here aborts startup; the process does not come up with a closed window. |
| Every retention pass | The current month plus the next two months, before any drop or delete. This only runs when `ALERT_RETENTION` is set, and only on the elected leader. A failure logs `ensure alert partitions failed` and skips that pass's drops and deletes. |

`EnsureAlertPartitions` is idempotent. When the default partition already holds
rows for a month that is about to get its own child, it detaches the default,
creates the monthly child, moves the matching rows into it, deletes them from
the default, and reattaches the default, all in one transaction. The move only
covers the months in the requested window; rows that landed in `alerts_default`
for an older month stay there unless retention's row-delete path removes them.

**Retention disabled means no hourly partition creation.** With
`ALERT_RETENTION` unset there is no prune loop, so the only creation points are
startup and the initial runway from migration `0011`. A deployment that runs
longer than that runway without a restart or retention enabled will eventually
write alerts into `alerts_default`. Restarting the service runs the startup
ensure again; rows for the current month move into the new child. Rows for
months that are already in the past stay in `alerts_default` (retention's
row-delete path can remove them once they are older than the cutoff). There is
no separate partition-management command in this release.

## What happens if creation falls behind

It does not cause an alert-insert outage. `alerts_default` is a real `DEFAULT`
partition, so a row for a month with no child is accepted there instead of
being rejected. When `EnsureAlertPartitions` later creates that month, it moves
the row into the new child.

The operational risk is an oversized default partition. Creating the missing
month requires detaching the default, moving every matching row, and
reattaching it under locks, which can take much longer than creating an empty
partition. The insert-failure symptom only appears if `alerts_default` has been
removed or detached; do not drop it. A missing monthly partition alone is not
an outage.

## Monitoring partition creation

There is no dedicated Prometheus metric for partition creation in this release.
The signals are:

- the error log line `ensure alert partitions failed` (a startup failure aborts
  startup; a prune-pass failure skips that pass);
- the error line `drop expired alert partitions failed`, which stops the pass
  before it reaches row deletes;
- the error line `alert archive failed; retention delete skipped`, which leaves
  the batch in the database for the next attempt;
- the info line `alert retention pruner started` with `retention`, `interval`,
  `batch`, and `archiving`, which confirms the hourly loop is running;
- the per-pass `alert retention prune` line with `deleted` and `cutoff`.

Also watch the default partition. This query lists the three months startup and
each prune pass are responsible for; every `partition` value should be
non-null:

```sql
SELECT to_char(m, 'YYYY_MM') AS month,
       to_regclass('alerts_' || to_char(m, 'YYYY_MM')) AS partition
  FROM generate_series(
         date_trunc('month', now()),
         date_trunc('month', now()) + interval '2 months',
         interval '1 month'
       ) AS m;
```

`SELECT count(*) FROM alerts_default;` should normally be zero or tiny. A
growing count means partition creation is behind. Alert on the error log and
run the query on a schedule when retention is disabled, since the hourly ensure
does not exist in that mode.

## Listing, indexes, and the composite key

The alert list is a search over history. `GET /api/v1/alerts` accepts
`from`/`to` (RFC 3339), which become `created_at` bounds. Postgres uses those
bounds to prune partitions; without them the query touches every child
partition. The parent indexes (`monitor_id`, `rule_id`, `contract_id`,
`created_at`, and `ledger` shapes) are created on every child, so a query
spanning N months costs roughly N partition index scans. Put a date bound on
dashboards and exports that do not need all history.

The primary key is `(id, created_at)`, not `id`, because a partitioned table's
unique constraint must include the partition key. A foreign key referencing
`alerts` must therefore use both columns. The schema already does this:
`delivery_attempts`, `alert_escalations`, and `dead_letters` carry
`alert_created_at` and reference `(id, created_at)`. A new table that writes
`REFERENCES alerts(id)` will fail with "there is no unique constraint matching
given keys"; add the companion timestamp and a composite foreign key.

`alert_dedup` is separate because `(rule_id, event_id)` must stay globally
unique and cannot include the partition key. Retention clears a deleted alert's
dedup key with the alert, so the key cannot block that event forever. A
federated alert — one another instance forwarded — has no local rule, so its
key is `(federation_origin, event_id)` instead, and the table carries both
keys as partial unique indexes.

## Backups and archiving

A `pg_dump` is the only database-level copy. If retention has already dropped a
month, a dump taken afterwards has no rows for it. Restoring a dump taken
before the drop brings that month back; restoring a later dump cannot. A
`sorobeacon backup` snapshot is configuration only and never includes alerts.

`ARCHIVE_URL` is the supported pre-delete copy:

- When an archive is configured, retention skips partition drops and uses the
  archive-then-delete path for every expired batch. A failed archive blocks that
  batch's delete, so nothing in the batch is deleted un-archived.
- Objects are NDJSON, one alert per line, at
  `alerts/<utc-day>/<firstID>-<lastID>.ndjson` under the configured prefix.
  Re-running the same batch overwrites the same key.
- There is no built-in import command. To restore, read the NDJSON with your own
  tooling; SoroBeacon does not query the archive.
- Because partition drops are skipped in archive mode, expired monthly
  partitions remain attached as empty shells after their rows are deleted. The
  partition count therefore grows; the rows themselves are gone.

## Per-workspace retention policies

In multi-tenant deployments, each workspace can override the instance-wide
`ALERT_RETENTION` window via `GET /api/v1/retention`, `PUT /api/v1/retention`,
and `DELETE /api/v1/retention`:

- **Effective policy resolution**: When a workspace has an explicit override in
  `workspace_retention_policies`, its `policy_source` is `workspace`.
  When no override is set (or after `DELETE /api/v1/retention`), the workspace
  inherits `ALERT_RETENTION` with `policy_source: "instance"` (or `"unset"` when
  neither a workspace policy nor `ALERT_RETENTION` is configured).
- **Shortening confirmation guardrail**: Lowering a workspace's effective
  retention window (for example from `90d` to `7d`, or resetting a `90d`
  override when the instance default is `7d`) permanently prunes older alerts on
  the next retention pass. The API rejects unconfirmed shortenings with
  `409 CONFLICT` (`confirmation_required: true`) unless `confirm_shorten: true`
  (or `?confirm_shorten=true`) is supplied. Every policy change or reset is
  recorded in `audit_log` under `target_type = "workspace_retention"`.
- **Per-workspace pruning & shared partition lifecycle**: During each prune
  pass, `RunAlertPruner` resolves the effective retention window for every
  workspace and deletes expired rows per workspace in bounded batches,
  cascading to `delivery_attempts`, `alert_escalations`, `dead_letters`, and
  `alert_dedup`. Because monthly Postgres partitions (`alerts_YYYY_MM`) are
  shared across workspaces, a monthly partition is only detached and dropped
  when its upper bound is older than the **longest** effective retention horizon
  across all workspaces (`MaxEffectiveRetention`). Workspaces with shorter
  retention windows still have their expired rows removed immediately via batched
  per-workspace `DELETE`s and read-time cutoff filtering.
- **Query & export horizon metadata**: Alert queries, stats, and exports enforce
  each workspace's active retention cutoff at read time (`created_at >= now -
  effective_retention`) so expired rows are never returned between hourly pruner
  ticks. `GET /api/v1/alerts`, `GET /api/v1/stats`, `GET /api/v1/stats/alerts-daily`,
  `GET /api/v1/alerts.csv`, and `GET /api/v1/alerts/export` expose the active
  cutoff via `pruned_before` (and `X-Pruned-Before` / `X-Retention-Policy-Source`
  response headers).
- **Observability**: Pruner passes log structured per-workspace attributes
  (`workspace_id`, `policy_source`, `retention`, `deleted`, `cutoff`) and export
  `sorobeacon_alert_pruner_deleted_total{workspace_id,policy_source}` and
  `sorobeacon_workspace_retention_seconds{workspace_id,policy_source}`.

## Choosing a window

Choose the shortest window that still supports incident review and audit
needs. Table size grows with alert volume times the window. Monthly partitions
make whole-month removal cheap, but the current month and the month containing
the cut-off are deleted row by row in 1000-row batches. The first pass is
immediate; later passes run hourly, so an expired row can remain for up to an
hour, longer if the leader is down or a pass keeps failing.

Typical starting points are `30d` for recent incident context, `90d` for a
quarter, and `365d` only with archive storage or capacity planning. If you need
history beyond the database window, set `ARCHIVE_URL` and keep the database
window short.

## Enabling retention on an existing deployment

Migration `0011_alerts_partitioning` is an in-place rewrite: it reads and
rewrites the whole `alerts` table once under an `ACCESS EXCLUSIVE` lock. On a
table with millions of rows, schedule it in a maintenance window. The migration
also creates the initial year of partitions. After upgrading, set
`ALERT_RETENTION` and restart. The first prune pass runs immediately; there is
no dry run, so lower the value deliberately.

See also [poll priority, reorgs, retention and archiving](retention-and-reorg.md),
[backing up and restoring the database](backup-restore.md), and the
[configuration reference](../configuration.md).
