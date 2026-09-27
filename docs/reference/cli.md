# Command line

`sorobeacon` is one long-running process with **no command-line flags and no subcommands**. Anything you would expect to be a flag is an environment variable instead.

## Synopsis

```sh
sorobeacon [arguments...]
```

`cmd/sorobeacon/main.go` imports neither the `flag` package nor a CLI framework, and nothing in the binary reads `os.Args`. Arguments on the command line are therefore **ignored** — they neither configure the process nor print usage, and they never cause a bad-argument error.

That includes `--help`. Verified by running the binary with a configured environment:

```console
$ HTTP_ADDR=:9099 DATABASE_URL='postgres://...:5432/sorobeacon?sslmode=disable' ./bin/sorobeacon --help
{"time":"2026-09-24T03:06:37.464167393Z","level":"INFO","msg":"configuration loaded","database_url":"postgres://localhost:5432/sorobeacon","http_addr":":9099","source_mode":"rpc","poll_interval":"5s","log_level":"info","network":"testnet","rpc_url":"https://soroban-testnet.stellar.org","sorotrail_url":"","cors_allowed_origins":""}
{"time":"2026-09-24T03:06:37.481Z","level":"INFO","msg":"database ready"}
{"time":"2026-09-24T03:06:37.6Z","level":"INFO","msg":"network verified","network":"testnet","rpc_url":"https://soroban-testnet.stellar.org"}
{"time":"2026-09-24T03:06:37.720115386Z","level":"INFO","msg":"poller started","interval":5000000000}
{"time":"2026-09-24T03:06:37.72014741Z","level":"INFO","msg":"http server listening","addr":":9099","rpc_url":"https://soroban-testnet.stellar.org"}
```

Middle lines abbreviated; note the logged `database_url` has its credentials stripped — that is `config.LogAttrs`, not this page eliding them.

Instead of a usage screen, the server starts. Stop it with `Ctrl-C`.

## Flags

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| _(none defined)_ | | | The binary defines no flags. |

There is also no `--version` flag: version, commit and build date are baked in at compile time (`-ldflags` in the `Makefile` and the `Dockerfile`) and served over HTTP at `GET /api/v1/version`.

## Flags versus environment variables

There is no precedence to resolve — the environment is the only configuration channel, so nothing can override it. [Environment variable reference](../configuration.md) lists every variable `internal/config` reads, with types, defaults and which are required.

Verified by running the binary: an ignored argument left configuration exactly as the environment set it (`POLL_INTERVAL=100ms ./bin/sorobeacon --poll-interval=1s` still failed with the `POLL_INTERVAL` error below, because the environment value was the one in play).

| To change | Do this |
| --- | --- |
| RPC endpoint | `RPC_URL=https://soroban-mainnet.stellar.org ./bin/sorobeacon` |
| Listen address | `HTTP_ADDR=127.0.0.1:9090 ./bin/sorobeacon` |
| Poll cadence | `POLL_INTERVAL=15s ./bin/sorobeacon` |
| Log verbosity | `LOG_LEVEL=debug ./bin/sorobeacon` |
| Database | `DATABASE_URL=postgres://... ./bin/sorobeacon` (required) |

## Exit codes

| Code | When | How it was verified |
| --- | --- | --- |
| `0` | Clean shutdown after `SIGINT` (Ctrl-C) or `SIGTERM` | Sent `SIGTERM` to a running process → log line `shutting down` → exit `0` |
| `1` | Any fatal error: configuration load, migration, database connection, network-passphrase mismatch, or the HTTP listener failing | Ran with a missing `DATABASE_URL`, with `POLL_INTERVAL=100ms`, and with `HTTP_ADDR` already bound → exit `1` each time |

Real fatal messages, with the environment that produced them:

| Message | Cause |
| --- | --- |
| `DATABASE_URL is required (e.g. postgres://user:pass@localhost:5432/dbname?sslmode=disable)` | `DATABASE_URL` unset or empty |
| `POLL_INTERVAL "100ms" is below the 1s minimum` | `POLL_INTERVAL` below one second |
| `listen tcp :8080: bind: address already in use` | `HTTP_ADDR` port already taken |
| `network mismatch: configured for mainnet ("Public Global Stellar Network ; September 2015") but the RPC endpoint belongs to testnet ("Test SDF Network ; September 2015") — check NETWORK / RPC_URL` | `RPC_URL` points at a different network than `NETWORK` says |

An RPC endpoint that is merely **unreachable** at startup is not fatal: the mismatch check logs `could not verify network passphrase` as a warning, startup health logs `event source health check failed at startup`, and the process keeps running — the poller retries with backoff. Only a *confirmed* passphrase mismatch stops the process.

Every fatal line is logged with the message `fatal`. The stream depends on when the failure happens, which matters if you separate them in a supervisor:

* Failures during configuration load — before the JSON logger is installed — go to **stderr** in Go's default text format:
  `2026/09/24 03:04:26 ERROR fatal err="DATABASE_URL is required ..."`
* Everything after startup goes to **stdout** as structured JSON:
  `{"time":"...","level":"ERROR","msg":"fatal","err":"listen tcp :8080: bind: address already in use"}`

## Signals

| Signal | Effect |
| --- | --- |
| `SIGINT` / `SIGTERM` | Graceful: logs `shutting down`, stops the poller, drains HTTP connections for up to 10s, exits `0`. |
| `SIGHUP` | Not handled specially. Configuration is read **once at startup** — to change an environment variable, restart the process. |
| `SIGKILL` | Not catchable; the process dies without checkpointing. On the next start ingestion resumes from the stored checkpoint. |

## Realistic invocations

```sh
# Testnet against the compose Postgres (the Quickstart's database)
DATABASE_URL='postgres://sorobeacon:sorobeacon@localhost:5432/sorobeacon?sslmode=disable' ./bin/sorobeacon

# Mainnet through your own RPC, bound to loopback, quieter logs
NETWORK=mainnet \
RPC_URL=https://soroban-mainnet.stellar.org \
DATABASE_URL='postgres://beacon:...@db.internal:5432/beacon?sslmode=disable' \
HTTP_ADDR=127.0.0.1:9090 \
LOG_LEVEL=warn ./bin/sorobeacon

# The repository's own workflow: build then run
make run

# Load a .env file first, then run (flags are pointless, env is the config)
set -a; . ./.env; set +a; ./bin/sorobeacon
```

Running it in containers instead? See [Running SoroBeacon with Docker Compose](../getting-started/docker.md) — the compose file supplies the same environment variables to the container.

## Checking that it came up

```sh
curl -s localhost:8080/api/v1/health    # db + rpc status, ledger lag
curl -s localhost:8080/api/v1/version   # version/commit/date baked in at build
curl -s localhost:8080/api/v1/stats     # counts + last poll time
```

See [HTTP API](api.md) for the full endpoint list.
