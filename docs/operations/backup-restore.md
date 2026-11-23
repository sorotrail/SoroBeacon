# Backing up and restoring the database

SoroBeacon keeps **all of its state in Postgres**: monitors, rules,
notification channels, their attachments, and the alert and delivery history
the dashboard shows. Everything else — the binary and its environment
configuration — can be recreated. This page is the short operational
version: what to back up, the commands to do it, and the one ordering rule
that matters when restoring.

## Which backup do you need?

| You want to… | Use | Covers |
| --- | --- | --- |
| Recover the whole instance after losing the database | `pg_dump` (full archive) | Everything, including alert and delivery history |
| Move a monitoring setup to new infrastructure | `sorobeacon backup` (JSON snapshot) | Monitors, rules, channels, saved searches, templates |
| Restore configuration onto a populated instance | `sorobeacon restore --force` | Appends the snapshot's rows; existing rows are kept |
| Keep alert history for a compliance window | `pg_dump` + [retention and archiving](retention-and-reorg.md) | Everything retention has not already dropped |

The two mechanisms are not interchangeable: the snapshot carries
configuration only, and the archive carries the whole database.

## What is stateful, and what is not

| State | Where it lives | How to back it up |
| --- | --- | --- |
| Monitors, rules, channels, attachments | Postgres | `pg_dump` (below) |
| Alert and delivery history | Postgres (`alerts`, `delivery_attempts`) | `pg_dump` (below) |
| Migration checkpoint | Postgres (`schema_migrations`) | travels with `pg_dump` |
| Environment config (`RPC_URL`, `API_TOKEN`, `POLL_INTERVAL`, …) | environment / `.env` | your config management |
| The binary or image | build artefact | `docker compose build` / `make build` |

There is one exception worth calling out: **`CONFIG_ENCRYPTION_KEY`**. When
channel config encryption is enabled, the channel credentials inside
`channels.config` are only as recoverable as that key — store the `.env`
holding it together with your database backups. A restored database without
its key still starts, but reading a channel's config fails with an error
naming the channel, and the config cannot be recovered from the dump alone.

## Taking a backup

A single compressed archive of the whole database, written from inside the
compose Postgres container:

```sh
docker compose exec -T postgres pg_dump --username sorobeacon --dbname sorobeacon --format=custom > sorobeacon-$(date +%F).dump
```

`--format=custom` is the archive format `pg_restore` reads; it is
compressed and supports restoring the whole database. Before relying on a
backup, check that it really contains the tables:

```sh
docker compose exec -T postgres pg_restore --list < sorobeacon-$(date +%F).dump | head
```

The listing shows one `TABLE DATA` entry per table, including
`schema_migrations`. If Postgres runs on the host instead of in compose,
the same `pg_dump` and `pg_restore` invocations apply without the
`docker compose exec` wrapper.

## Configuration-only backups

`alerts` and `delivery_attempts` are history: they grow with every match,
while configuration changes rarely. If a backup only needs to capture the
setup — monitors, rules, channels — exclude the history rows:

```sh
docker compose exec -T postgres pg_dump --username sorobeacon --dbname sorobeacon --format=custom \
  --exclude-table-data=alerts --exclude-table-data=delivery_attempts \
  > sorobeacon-config-$(date +%F).dump
```

`--exclude-table-data` omits only the rows, not the tables: a restore still
creates empty `alerts` and `delivery_attempts`, so foreign keys and future
history stay intact. Exclude the two **together** — every
`delivery_attempts` row references an `alerts` row, so keeping one without
the other would produce a dump that cannot be restored. The configuration
tables (`monitors`, `rules`, `channels`, `monitor_channels`), the
single-row `ingest_state` checkpoint and `schema_migrations` all stay in
the dump; the last one is tiny and must survive for the check below.

## Configuration snapshots with the CLI

`pg_dump` is the full-fidelity backup. For moving configuration between
instances, the binary also has a JSON snapshot pair:

```sh
sorobeacon backup > sorobeacon-config.json
sorobeacon restore < sorobeacon-config.json
```

> **The snapshot is a credential file.** Channel `config` is decrypted in the
> output, so the file can restore onto an instance with a different (or no)
> `CONFIG_ENCRYPTION_KEY` — but that also means webhook URLs, bot tokens and
> SMTP passwords appear in it in plaintext. Protect it like the database dump
> or the encryption key: restrictive permissions, encrypted at rest, and
> deleted when no longer needed. The command refuses to write to a terminal,
> and that is the only safeguard it can enforce.

The snapshot contains monitors (with their channel attachments), rules,
channels (with config), saved searches and monitor templates. It does **not**
contain alerts, delivery attempts, dead letters, escalation state, maintenance
windows, inhibitions, the audit log, scoped tokens or the ingest checkpoint —
use `pg_dump` when you need history. It also covers only the **default
workspace**; configuration in other workspaces is not exported.

### Verifying a snapshot

A snapshot is a JSON file, so it can be checked without a database:

```sh
jq '{version, created_at, monitors: (.monitors | length), rules: (.rules | length),
     channels: (.channels | length), saved_searches: (.saved_searches | length),
     templates: (.templates | length)}' sorobeacon-config.json
```

The counts are the configuration the file carries. `version` is the snapshot
format version (currently `1`); a file whose `version` is higher than the
binary's is refused, and a file with a lower one is not.

### What a restore does to existing data

**A restore appends. It never deletes or updates an existing row.** On a
non-empty database it refuses unless `--force` is passed:

```sh
sorobeacon restore < sorobeacon-config.json
# ERROR restore failed err="database already contains 3 monitors, 5 rules, 2 channels, 1 saved searches and 0 templates; rerun with --force to append the backup (existing rows are kept)"
```

With `--force` the backup's rows are added **alongside** the existing ones
with new IDs (the original `created_at` timestamps are kept), and every
cross-row reference — monitor attachments, rule ownership, saved-search
filters and template channel lists — is remapped to the new IDs. Nothing is
overwritten: two monitors named `token-watch` end up as two monitors, and
both alert.

To *replace* an instance's configuration, restore into an empty database.
There is no confirmation step and no "replace" mode: take a `pg_dump`
first if the current configuration is worth keeping.

### Dry run first

`--dry-run` validates the file and reports the same row counts and the same
accept/refuse decision as a real run, without writing anything:

```sh
sorobeacon restore --dry-run --force < sorobeacon-config.json
```

```json
{"level":"INFO","msg":"dry run: no rows written","mode":"append",
 "backup_version":1,"backup_created_at":"2026-10-20T22:04:35Z",
 "monitors_in_backup":1,"rules_in_backup":1,"channels_in_backup":1,
 "saved_searches_in_backup":1,"templates_in_backup":0,
 "existing_monitors":1,"existing_rules":1,"existing_channels":1,
 "existing_saved_searches":1,"existing_templates":0,"existing_alerts":0}
```

* `mode` is `into-empty-database` when the target holds no configuration and
  `append` when it does — the same decision the real run makes.
* `*_in_backup` are the counts the file carries; `existing_*` are the counts
  the target already holds.
* `existing_alerts` is reported for information only: history does not
  collide with configuration, so it never forces `--force`.

The refusal for a non-empty database is checked **before** the dry run, so
`--dry-run` without `--force` exits with the same refusal (exit code 2) a
real run would. Add `--force` to preview a restore into a populated
database.

### Validation, exit codes and versions

A malformed, truncated or internally inconsistent file is refused before the
database is opened, so it creates no schema and writes no rows. Validation
covers the backup version, required timestamps, duplicate IDs, dangling
monitor and rule references, and config shapes. An import that fails against
the database part-way through is **not transactional**: rows written before
the failure remain, and the next restore appends to them.

| Exit code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Backend failure (database unreachable, write rejected) |
| 2 | Usage error (bad flag, unexpected argument, restore needs `--force`, backup aimed at a terminal) |
| 3 | Invalid backup (not JSON, truncated, inconsistent, or newer than this binary) |

**Version compatibility.** The file carries a numeric `version`. A file
written by a *newer* SoroBeacon is refused with exit code 3 — upgrade the
binary before restoring it. A file written by an older version is accepted,
so a rollback can restore a pre-upgrade snapshot. Because the snapshot
carries configuration and no alert rows, the `alerts` table's partitioning
and its composite primary key never come into it: those matter for
`pg_dump`, not for a snapshot.

### Where to keep snapshots, and for how long

A snapshot is a credential file (see the warning above) *and* the only copy
of a hand-built monitoring setup, so it deserves the same care as the
database dump:

* keep it somewhere the whole team can reach, but not in a world-readable
  location — `chmod 600` at minimum;
* store it encrypted at rest (the object store's own encryption, or a
  client-side encrypted bucket), because a snapshot is plaintext on disk;
* **never** commit one to a repository, and never attach one to a ticket.
  A snapshot carries live webhook URLs and SMTP passwords;
* keep at least the last few, and one per configuration change worth
  remembering. There is no expiry to lean on: unlike alert history,
  configuration does not age out;
* delete a snapshot as soon as the setup it holds has been superseded — it
  is a live credential, not a log.

The alert history a snapshot does **not** carry is governed by
[retention and archiving](retention-and-reorg.md): expired `alerts`
partitions are dropped wholesale, and the rows inside the current month are
deleted row by row. A snapshot taken today therefore cannot recover history
that retention has already dropped — use the archive for that.

## Worked procedures

### Moving an instance to new infrastructure

The goal is the same setup on new hardware, with the alert history left
behind (a snapshot has none).

1. On the old instance, take a snapshot:

   ```sh
   DATABASE_URL=postgres://sorobeacon:sorobeacon@old-db:5432/sorobeacon \
     sorobeacon backup > sorobeacon-config.json
   ```

2. Move the file to the new instance over a channel you trust, and keep it
   readable only by the operator account (`chmod 600`).

3. On the new instance, create the database empty and let the binary's
   migrations build the schema. `sorobeacon restore` opens the store with
   the migrations applied, so the schema is created for you:

   ```sh
   DATABASE_URL=postgres://sorobeacon:sorobeacon@new-db:5432/sorobeacon \
     sorobeacon restore --dry-run < sorobeacon-config.json
   DATABASE_URL=postgres://sorobeacon:sorobeacon@new-db:5432/sorobeacon \
     sorobeacon restore < sorobeacon-config.json
   ```

   The dry run is safe on an empty database — the guard only refuses when
   the target already holds configuration.

4. Copy the environment configuration across by hand: `RPC_URL`,
   `API_TOKEN`, `POLL_INTERVAL`, the network list, and
   `CONFIG_ENCRYPTION_KEY` if you use one. The snapshot carries channel
   configs in plaintext, so a new encryption key is fine: the restored
   channels are written through the store, which encrypts them with the
   target's key as they are restored.

5. Start the service and check that it alerts:

   ```sh
   curl -s localhost:8080/api/v1/health
   curl -s -X POST localhost:8080/api/v1/channels/1/test
   ```

   The channel test is the one that proves the SMTP credentials survived the
   move.

### Recovering from a lost database

The database is gone and the archive is the only thing left. This is the
`pg_dump` path, because history is part of what was lost.

1. Stop the service, so nothing writes while you restore:

   ```sh
   docker compose stop sorobeacon
   ```

2. Recreate the database empty and restore the archive into it:

   ```sh
   docker compose exec -T postgres dropdb --username sorobeacon --if-exists sorobeacon
   docker compose exec -T postgres createdb --username sorobeacon sorobeacon
   docker compose exec -T postgres pg_restore --username sorobeacon --dbname sorobeacon < sorobeacon-$(date +%F).dump
   ```

3. Check the row counts against what the instance had:

   ```sh
   docker compose exec -T postgres psql -U sorobeacon -d sorobeacon -c \
     "SELECT (SELECT count(*) FROM monitors) AS monitors, (SELECT count(*) FROM rules) AS rules, (SELECT count(*) FROM channels) AS channels, (SELECT count(*) FROM alerts) AS alerts, (SELECT count(*) FROM delivery_attempts) AS attempts, (SELECT version FROM schema_migrations) AS migrated_to;"
   ```

4. Start the service again:

   ```sh
   docker compose up --build -d
   curl -s localhost:8080/api/v1/health
   ```

If the archive is gone but a snapshot survives, the same procedure applies
to the snapshot instead — with the difference that the alert history is
gone for good, and that the restore **appends** to whatever the database
already holds. Restore the snapshot into an empty database, never over a
populated one.

## Restoring

Restore into an **empty database, with the service stopped**. Substitute
the date in the file name with the one you are restoring.

1. Stop the service:

   ```sh
   docker compose stop sorobeacon
   ```

2. Recreate the database empty:

   ```sh
   docker compose exec -T postgres dropdb --username sorobeacon --if-exists sorobeacon
   docker compose exec -T postgres createdb --username sorobeacon sorobeacon
   ```

3. Restore the archive into it:

   ```sh
   docker compose exec -T postgres pg_restore --username sorobeacon --dbname sorobeacon < sorobeacon-$(date +%F).dump
   ```

   A configuration-only archive restores with exactly this command into an
   empty database as well.

4. Confirm the row counts match the backup:

   ```sh
   docker compose exec -T postgres psql -U sorobeacon -d sorobeacon -c \
     "SELECT (SELECT count(*) FROM monitors) AS monitors, (SELECT count(*) FROM rules) AS rules, (SELECT count(*) FROM channels) AS channels, (SELECT count(*) FROM alerts) AS alerts, (SELECT count(*) FROM delivery_attempts) AS attempts, (SELECT version FROM schema_migrations) AS migrated_to;"
   ```

5. Start the service again:

   ```sh
   docker compose up --build -d
   curl -s localhost:8080/api/v1/health
   ```

## Migrations on first start

There is no separate migrate command to run. Migrations are embedded in the
binary, and the service applies any pending ones itself during startup,
before it accepts traffic.

A restored archive carries `schema_migrations` along with the schema, so
the first start after a restore finds the database already at the version
the backup was taken with and applies nothing — only a backup older than
the running binary gains new migrations, and only the missing ones run.

**Order matters: restore before starting the service.** Starting first
against the empty database lets migrations create the whole schema, which
`pg_restore` then collides with, and a migration running *while* a restore
writes tables can leave `schema_migrations` marked dirty and the service
refusing to start. The safe order is always: stop, drop and create empty,
restore, then start.
