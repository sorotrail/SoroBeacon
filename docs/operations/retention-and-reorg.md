# Poll priority, reorgs, retention and archiving

Four behaviours that decide how SoroBeacon behaves under load and under a
reorganised chain. All four are off by default or default to the previous
behaviour, so upgrading does not change what an existing deployment does.

## Poll priority

Each monitor has a `priority` of `low`, `normal` (the default) or `high`,
settable on `POST`/`PATCH /api/v1/monitors`. The poller orders each cycle's
watch list so high-priority contracts are fetched in an earlier `getEvents`
request instead of waiting behind a batch of low-traffic ones.

Ordering is a weighted round-robin over the tiers (high 4, normal 2, low 1 per
round). Every tier appears in the first round, so the lowest-priority contract
is never more than the sum of the weights — seven contracts — behind, no
matter how many high-priority contracts are queued. A per-tier rotation cursor
also stops the same contract being permanently last within its tier. A
contract watched by monitors of different priorities is scheduled at the
highest priority among them.

`GET /metrics` exposes `sorobeacon_poll_priority_contracts` (contracts per
tier) and `sorobeacon_poll_lag_ledgers_by_priority` (how stale the freshest
event seen for each tier is this cycle).

## Reorg detection

Reporting an event that did not happen is the worst failure a monitoring tool
can have, so the poller records the hash of every recently ingested ledger and
re-reads that window each cycle (`REORG_TRACKING_WINDOW`, default 128 ledgers).
A ledger whose hash changed marks the alerts derived from the orphaned range as
**retracted**: kept in the table and flagged (`retracted_at`), never deleted,
because a delivered notification cannot be unsent. The dashboard shows a
"retracted" badge and the alert detail explains which ledger was orphaned.
Reorgs are logged at `warn` with the ledger range and counted in
`sorobeacon_reorgs_total`; the checkpoint is rewound to just before the
divergence so the replacement chain is re-read.

`REORG_CONFIRMATION_DEPTH` (default 0) makes the poller hold an event until it
is that many ledgers behind the tip before evaluating it. Raising it trades
alert latency for far fewer retractions. `REORG_TRACKING_WINDOW=0` disables
detection entirely.

Detection needs ledger hashes, which come from the RPC's `getLedgers`. A
SoroTrail source, or an RPC node too old to answer `getLedgers`, simply runs
without detection.

## Partitioned alerts and retention

On Postgres, `alerts` is range-partitioned by `created_at`, one partition per
UTC month plus a `DEFAULT` partition that stores a row whose month has no
partition yet (so a missing partition never loses a row). Upcoming months are
created on startup and on every prune pass.

With `ALERT_RETENTION` set, retention drops whole expired partitions — which is
effectively free compared to deleting rows — and clears the delivery attempts
and dedup keys for the dropped alerts in the same transaction, since a `DROP`
does not fire `ON DELETE CASCADE`. The current (partial) month is deleted row
by row in batches of 1000, the ragged edge.

Two consequences worth knowing before you set it:

- The dedup guard (`rule_id`, `event_id`) lives in a small `alert_dedup` table,
  because a partitioned table cannot carry a global unique index that omits the
  partition key. Retention clears both together so a deleted alert's key cannot
  block that event forever.
- `delivery_attempts` references `alerts (id, created_at)` — a composite key,
  the only unique key a partitioned table can offer — so the cascade survives.
  Row deletes cascade as before; partition drops remove children explicitly.

**Migrating an existing deployment** is an in-place rewrite: the `0011`
migration reads and rewrites the whole `alerts` table once under an
`ACCESS EXCLUSIVE` lock. On a table with millions of rows, schedule it in a
maintenance window. SQLite is not partitioned — a single file has no
per-partition storage to drop — and keeps batched deletes.

## Archiving before deletion

Set `ARCHIVE_URL` (a directory, `file://`, `dir://` or `s3://bucket/prefix`)
and retention copies each batch of expired alerts to NDJSON before deleting it.
A failed archive blocks that batch's delete, so nothing is dropped
un-archived — the ordering is the guarantee. Objects are keyed by the batch's
own id range, so re-running after a crash overwrites rather than duplicates.

Archiving is off by default, and it is skipped in favour of partition drops
when it is on: a `DROP` cannot archive, so with `ARCHIVE_URL` set the row-by-row
archive-then-delete path is used instead. Archive objects contain only alert
rows — contract data, never channel `config`, so a webhook URL or bot token can
never reach one.
