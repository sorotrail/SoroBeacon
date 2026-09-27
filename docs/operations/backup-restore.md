# Backing up and restoring the database

SoroBeacon keeps **all of its state in Postgres**: monitors, rules,
notification channels, their attachments, and the alert and delivery history
the dashboard shows. Everything else — the binary and its environment
configuration — can be recreated. This page is the short operational
version: what to back up, the commands to do it, and the one ordering rule
that matters when restoring.

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
