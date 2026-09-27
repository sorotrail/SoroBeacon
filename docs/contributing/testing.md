# How to run the test suite

`CONTRIBUTING.md` and the [development guide](development.md) cover setup.
This page covers the thing that actually costs contributors time: **the
Postgres-backed tests skip silently when `TEST_DATABASE_URL` is unset**, so a
green local `go test ./...` does not prove the store paths work. Read
[The trap](#the-trap-skip-without-test_database_url) before your first PR.

## What CI runs

CI (`.github/workflows/ci.yml`) runs three jobs on every PR:

| Job | What it runs | Needs a database? |
| --- | --- | --- |
| `build` | `go build ./...`, `go vet ./...`, `go test ./...`, then the SQLite store conformance suite by name (`go test ./internal/store/ -run TestSQLite -v`) | No — the SQLite suite runs against a temp file |
| `test-db` | `go test ./...` again, with `TEST_DATABASE_URL` set against a Postgres 16 service container | Yes |
| `lint` | `golangci-lint run` (v2 config, `.golangci.yml`) | No |

A local `go test ./...` with no `TEST_DATABASE_URL` exercises everything the
`build` job does, and **skips** what the `test-db` job runs. So the local run
that feels equivalent to CI is the *full* `build` + `test-db` + `lint` set,
not `go test ./...` alone.

## The trap: skip without `TEST_DATABASE_URL`

Stated plainly: **without `TEST_DATABASE_URL`, the whole Postgres store
conformance suite (`internal/store/postgres_test.go`, `TestPostgresConformance`)
calls `t.Skip` before running anything.** You will see `go test ./...` pass and
`ok ... internal/store` with no hint that roughly half the store's assertions
never executed. The same applies to `make test`: it runs `go test ./...` and
therefore skips the same tests.

Then you open a PR, CI's `test-db` job runs those paths for the first time,
and a store change that only works on SQLite fails there — an hour after you
thought you were done.

To actually run everything locally:

```sh
make up          # starts the docker-compose stack, Postgres included
make test-db     # go test ./... with TEST_DATABASE_URL pointed at the compose Postgres
```

`make test-db` defaults `TEST_DATABASE_URL` to
`postgres://sorobeacon:sorobeacon@localhost:5432/sorobeacon?sslmode=disable`
(the Makefile fills it in when unset), which is exactly what `make up`
started. Any reachable Postgres works: point `TEST_DATABASE_URL` elsewhere to
use your own.

One more wrinkle: `make up` starts the **whole compose stack** — SoroBeacon
and Postgres. If you only want the database:

```sh
docker compose up -d postgres
```

Both engines, one suite: the store has a single behavioural conformance suite
(`internal/store/conformance_test.go`) that both backends run unchanged. The
SQLite side needs no service container, so it runs as part of plain
`go test ./...`; the Postgres side is the part that skips.

## The commands

From the repository root:

```sh
go build ./...   # everything compiles
go vet ./...     # static analysis CI also runs
go test ./...    # unit tests + SQLite store suite (Postgres suite skips)
make test-db     # the same, plus the Postgres store suite (needs `make up` first)
make lint        # golangci-lint run (CI's lint job)
```

`make cover` produces a coverage figure:

```sh
make cover
# go test -coverprofile=coverage.out ./...
# go tool cover -func=coverage.out
# ...tail of output: total: (statements) XX.X%
```

Read it as: the `total:` line at the bottom of `go tool cover -func` is the
figure; the per-function lines above it show where coverage is thin. For a
visual, `go tool cover -html=coverage.out` opens the annotated source in a
browser. Note the profile is produced **without** `TEST_DATABASE_URL` by
default, so store paths count as uncovered; run
`TEST_DATABASE_URL=... make cover` (the recipe honours the environment
variable through `go test`) if you are measuring store coverage.

## Running one package or one test

Standard `go test` scoping, which is the fastest loop while iterating:

```sh
go test ./internal/api/                     # one package
go test ./internal/rules/ -run TestCooldown # tests whose name matches
go test ./internal/store/ -run TestSQLiteConformance/AlertDedupAndListing -v
```

`-run` takes a regular expression; `/` selects subtests (the conformance
suite's `t.Run` names, like `AlertDedupAndListing` above). `-v` prints each
subtest as it runs — worth it on the store suite, which otherwise reports a
single `ok` line for ~25 subtests.

The store suite rebuilds state per subtest (`resetConformance`), so subtests
are independent; you can run any subset alone.

## Conventions in this repo

* **Table-driven tests with named cases.** The pattern is a slice of
  `struct { name string; ... }` iterated with `t.Run(tt.name, ...)` — see
  `TestSQLiteFilePath` in `internal/store/sqlite_test.go` or the validators'
  tests in `internal/rules`. Name cases after the behaviour they pin, not
  `case1`/`case2`.
* **`httptest` for anything HTTP.** API tests build a `chi` router via
  `s.Routes()` and serve it with `httptest.NewServer`, then issue real
  requests with `http.Post`/`http.Get` and assert on status codes and the
  JSON envelope — see `internal/api/maxbody_test.go` or
  `internal/api/auth_test.go`. Fakes implement the narrow store interface the
  handler needs; no database is involved.
* **`testify` in the store, stdlib elsewhere.** The store suite uses
  `require` (stop on failure) for setup and `assert` (collect failures) for
  the pins under test; most other packages use plain `t.Errorf`/
  `t.Fatalf`. Match the package you are in.
* **Tests skip, never fail, on missing externals.** Only the
  `TEST_DATABASE_URL` behaviour above skips today; everything else runs
  hermetically. If you add an external dependency to a test, follow the same
  pattern and say so in the PR description.
* **Rule types and channels must ship with unit tests** (`CONTRIBUTING.md`
  ground rule); store changes must be covered by the shared conformance suite
  so both backends get the assertions.
