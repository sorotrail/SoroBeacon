# Upgrading a running deployment

SoroBeacon is one Go binary (or one container image) with its schema
migrations embedded in it. There is no separate migrate step — and one
behaviour that surprises people mid-upgrade: **migrations run automatically
at startup, before the service accepts traffic.** This page describes what
that means in practice, the order to upgrade in, and what rollback honestly
looks like.

## Migrations run on startup

On every start, the process applies any pending schema migrations, then
finishes booting (`cmd/sorobeacon/main.go` calls
`internal/store/migrate.go`'s `Migrate` before the store is opened, before
the poller starts and before the HTTP listener binds). Consequences:

* An **up-to-date database is a no-op** — a restart that changes nothing
  applies nothing.
* A **newer binary migrates the database itself** the first time it boots.
  No operator action, but also no gate: the schema moves the moment the new
  process starts, not when you decide it should.
* Migrations are **embedded in the binary** (`internal/store/migrate.go`
  compiles the `internal/store/migrations/` set in with `go:embed`), so the
  binary you deploy carries exactly the migrations it needs. Postgres and
  SQLite each have their own parallel set; a deployment never runs DDL from
  the wrong backend.
* A partially-applied migration marks the database **dirty** in
  `schema_migrations`, and further starts refuse rather than guess. See
  [diagnosing](#telling-a-failed-migration-from-a-failed-start).

### What this means for a rolling deploy

While an upgrade is in flight you can briefly have **two versions running at
once** — the old process still serving, the new one starting beside it. With
automatic startup migrations, the version that boots first decides the
schema:

* The new process migrates *before* it serves, so by the time it takes
  traffic the schema is already the new one. The old process then runs
  against a schema one version ahead of its binary. That is supported for
  normal upgrades (the store is written to tolerate a forward-applied
  schema — existing queries keep working), but it is the window in which
  the old code is executing on new DDL.
* **Do not run two versions against one Postgres by design.** The
  single-file `ingest_state` checkpoint means two pollers would race the
  ingest cursor; the docs' scaling guidance is one instance per database.
  If you scale out for availability, both instances should be the **same
  version** — and even then the checkpoint makes a second poller redundant
  rather than helpful.
* The safe rolling pattern for a restart-manager (systemd, a container
  orchestrator) is the ordinary one: **one replacement at a time**. Stop or
  drain the old process, start the new one (it migrates), confirm health,
  move on. The overlap window is then seconds, not minutes.

## Checking the current schema version

The applied version is a single row in the `schema_migrations` table
(golang-migrate's bookkeeping). For the compose Postgres:

```sh
docker compose exec -T postgres psql --username sorobeacon --dbname sorobeacon \
  -c "SELECT version, dirty FROM schema_migrations;"
```

Against any other Postgres, run the same query with your connection:

```sh
psql "$DATABASE_URL" -c "SELECT version, dirty FROM schema_migrations;"
```

What you will see (verified against a real Postgres 16 with this
repository's migrations applied):

* `version` — the highest applied migration number (11 for the current
  `main`); it advances as the new binary migrates on startup.
* `dirty` — `f` normally. `t` means a migration started and did not finish;
  the database is mid-flight and further migrations are refused until it is
  resolved (see below).

`version` alone does not tell you whether it matches your *binary* — it
tells you the database's position. After an upgrade, `version` at the
current `main` value is the confirmation that the new binary's startup
migrations completed.

## The recommended order

Boring on purpose: **back up, stop, deploy, start.**

1. **Back up.** A migration that fails halfway is much calmer with a
   `pg_dump` taken minutes earlier — see
   [backing up](backup-restore.md). Back up `CONFIG_ENCRYPTION_KEY` with
   the database or encrypted channels will not decrypt after the restore.
2. **Stop the old process** (`docker compose stop sorobeacon`, or your
   supervisor's stop). No traffic is served during the swap; SoroBeacon is
   a monitor, so a short gap delays alerts rather than losing them (the
   poller resumes from its ledger checkpoint).
3. **Deploy the new binary/image** without starting it.
4. **Start**, and watch the first seconds of logs. This is where migrations
   run. `docker compose logs -f sorobeacon` or `journalctl -u sorobeacon`.
5. **Confirm**: `GET /api/v1/health` returns `200`, the startup line shows
   `database ready`, and `schema_migrations.version` matches the new
   release's expected number.

The stop-then-start order removes the rolling-deploy ambiguity above
entirely: migrations run against a database nothing else is writing to.

## Telling a failed migration from a failed start

Both end with the process exiting, but the logs differ:

| | Failed **migration** | Failed **start** after migrating |
| --- | --- | --- |
| Last log line | `fatal`, `err: apply migrations: ...` (or `init migrations`/`load migrations`) | anything after `database ready` — listener bind failure, config validation, RPC/network mismatch |
| `schema_migrations` | `dirty` is `t`, `version` at the failing migration | `dirty` is `f`, `version` at the new release |
| `/api/v1/health` | never serves | may briefly serve, then exit |
| Fix | see below | ordinary operational diagnosis: the new binary's config (env vars) or its dependencies (Postgres reachable, RPC reachable, network passphrase matches) |

Where to look, in order:

1. The process log — the failing migration is named in the error
   (`apply migrations: ...`).
2. The `schema_migrations` row (query above): `dirty`/`version` tells you
   the exact position.
3. Postgres's own log for the underlying SQL error — SoroBeacon wraps it,
   Postgres states it.

If a migration fails partway:

* **Restore the pre-upgrade backup** into an empty database (stop the
  service first — the ordering in [backing up](backup-restore.md)
  applies), then start the *old* binary. This is the supported path.
* Hand-editing `schema_migrations` (setting `dirty=false`, rewinding
  `version`) is golang-migrate's manual recovery mechanism, but it leaves
  the schema in whatever state the failed migration actually achieved.
  Only do it when you have read the failing migration's SQL and know the
  half-applied state is either complete or harmless — then re-run the new
  binary and let it retry.

## Rolling back

State it honestly: **downgrading the binary is not a supported rollback.
The `.down.sql` files exist for development, not for operators.**

* Every migration pair ships a `NNNN_name.down.sql`, and the migration
  framework can apply them — but **nothing in SoroBeacon's startup or
  operational surface ever does.** There is no command that rolls the
  schema back; running an older binary does not either, because the old
  binary simply finds an up-to-date database and starts (an up-to-date
  database is a no-op — it never *reverses* anything).
* The `.down.sql` files are a best-effort inverse of their `.up.sql`:
  they drop what was added. What they **do not** cover:
  * **Data already written into new columns/tables** — dropping the column
    discards it, and the alert rows dropped with a removed table cannot be
    reconstructed.
  * **Alert history semantics.** If an upgrade reshapes how matches are
    recorded (new payload fields, retraction markers), the old binary does
    not know about them; downgrading does not un-mark or un-reshape
    anything.
  * **Channel config written by newer code.** Config rows are forward- and
    backward-readable (the envelope is versioned), but a config value only
    newer config shapes accept will not round-trip through an old binary's
    validation.
* **The practical rollback for a bad upgrade** is the restore path from
  [backing up](backup-restore.md): stop, drop/recreate the database empty,
  restore the pre-upgrade `pg_dump` (which carries `schema_migrations` back
  at the old version), then start the **old** binary. Alert history between
  the backup and the rollback is lost; monitors, rules and channels come
  back as they were.
* If only *behaviour* regressed and the schema is fine, run the old binary
  against the migrated database — that is the one rollback that costs
  nothing. Verify the old release's health checks before declaring the
  rollback done, and report the regression as an issue rather than staying
  on an unsupported version pair.

## See also

* [Backing up and restoring the database](backup-restore.md) — the restore
  ordering and the `schema_migrations` details.
* [Architecture](../reference/architecture.md) — the two backends and their
  parallel migration sets.
* [Environment variable reference](../configuration.md) — if a start-up
  failure turns out to be configuration, this is the reference.
