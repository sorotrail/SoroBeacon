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
internal/store      Postgres (pgx) + SQLite backends, embedded migrations
internal/rules      rule registry + built-in evaluators
internal/notify     channel notifiers + retrying dispatcher
internal/poller     the ingest loop
internal/api        chi JSON API
internal/web        html/template + htmx dashboard
```

## Conventions

* Idiomatic Go: `gofmt`, `go vet`, and `golangci-lint run` must pass.
* Plain SQL in the store — no ORM. Migrations are sequential pairs in `internal/store/migrations` (`NNNN_name.up.sql` / `.down.sql`); the SQLite DDL lives in a parallel set under `internal/store/migrations/sqlite/` at the same version numbers. Never edit an applied migration.
* Structured logging via `log/slog`, lower\_snake\_case keys. **Never log channel config.**
* Tests: table-driven where natural. Rule types and channels must ship with unit tests. The store's shared conformance suite (`internal/store/conformance_test.go`) runs against both backends: SQLite as part of `go test ./...`, Postgres when `TEST_DATABASE_URL` is set (`make test-db`). A store change that is not reflected in both backends fails the suite.

## Pull requests

* Keep PRs focused; separate refactors from features.
* Describe the behavior change and how you tested it.
* New extension points get a doc comment telling the next contributor how to use them.

Licensed Apache-2.0. See `CONTRIBUTING.md` in the repo root for the full guidelines.
