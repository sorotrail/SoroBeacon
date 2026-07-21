# Development guide

## Setup

Go **1.25+** (the Stellar SDK dependency sets the floor) and Docker.

```sh
git clone https://github.com/khaylebfortune/sorobeacon.git
cd sorobeacon
docker compose up -d postgres     # just the database
cp .env.example .env
make build && make test
```

## Make targets

| Target | What it does |
| --- | --- |
| `make build` | `go build` → `bin/sorobeacon` |
| `make test` | Unit tests. Store integration tests **skip** without a database. |
| `make test-db` | All tests, including store integration tests against the compose Postgres (`TEST_DATABASE_URL` overridable). |
| `make lint` | `golangci-lint run` (v2 config). |
| `make up` / `make down` | Full docker-compose stack. |

## Layout

```
cmd/sorobeacon      wiring + graceful shutdown
internal/config     env config
internal/stellar    RPC client + ScVal decoder + value helpers
internal/store      Postgres (pgx) + embedded migrations
internal/rules      rule registry + built-in evaluators
internal/notify     channel notifiers + retrying dispatcher
internal/poller     the ingest loop
internal/api        chi JSON API
internal/web        html/template + htmx dashboard
```

## Conventions

* Idiomatic Go: `gofmt`, `go vet`, and `golangci-lint run` must pass.
* Plain SQL in the store — no ORM. Migrations are sequential pairs in `internal/store/migrations` (`NNNN_name.up.sql` / `.down.sql`); never edit an applied migration.
* Structured logging via `log/slog`, lower\_snake\_case keys. **Never log channel config.**
* Tests: table-driven where natural. Rule types and channels must ship with unit tests; store changes need integration tests (they read `TEST_DATABASE_URL` and skip when unset).

## Pull requests

* Keep PRs focused; separate refactors from features.
* Describe the behavior change and how you tested it.
* New extension points get a doc comment telling the next contributor how to use them.

Licensed Apache-2.0. See `CONTRIBUTING.md` in the repo root for the full guidelines.
