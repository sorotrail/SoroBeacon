# Contributing to SoroBeacon

Thanks for helping build monitoring infrastructure for the Soroban
ecosystem! SoroBeacon's MVP is intentionally small; most features are meant
to be added by contributors, and the codebase is organized around a few
interfaces to make that easy.

## Getting started

```sh
docker compose up -d postgres      # just the database
cp .env.example .env
make build && make test
make test-db                       # includes store integration tests
```

Go 1.25+ is required (the Stellar SDK dependency sets the floor).

## Where to plug in

| You want to add…        | Implement…             | Register in…                                   |
|-------------------------|------------------------|------------------------------------------------|
| a notification channel  | `notify.Notifier`      | `DefaultFactory` in `internal/notify/notify.go`|
| a rule type             | `rules.RuleEvaluator`  | `NewRegistry` in `internal/rules/rules.go`     |
| a different event source| `stellar.Client`       | wiring in `cmd/sorobeacon/main.go`             |
| a decoder (e.g. spec-aware) | `stellar.Decoder`  | wiring in `cmd/sorobeacon/main.go`             |
| another database        | `store.Store` (or a sub-interface) | wiring in `cmd/sorobeacon/main.go` |

The README has worked examples for channels and rules.

## Ground rules

- **Idiomatic Go.** `gofmt`, `go vet` and `golangci-lint run` must pass.
  Small, focused packages; interfaces at the boundaries; plain SQL in the
  store (no ORM).
- **Tests.** Table-driven where it fits. Rule types and channels must ship
  with unit tests; store changes need integration tests (they run against
  `TEST_DATABASE_URL` and skip when it's unset).
- **Never log or return secrets.** Channel `config` holds webhook URLs,
  tokens and SMTP credentials. Keep them out of log lines, error messages,
  API responses and delivery `response_snippet`s.
- **Migrations** are sequential files in `internal/store/migrations`
  (`NNNN_name.up.sql` / `.down.sql`); never edit an applied migration.
- **Structured logging** via `log/slog` with lower_snake_case keys.

## Good first issues

- Encrypt `channels.config` at rest (design note: an envelope-encryption
  interface in `internal/store` so the column stays opaque JSON).
- API authentication (token middleware on `/api/v1`).
- New rule types: absence-of-event ("no heartbeat for N minutes"),
  frequency ("more than N matches in M minutes").
- New channels: Matrix, PagerDuty, ntfy.sh.
- Contract-spec-aware decoding: fetch the contract spec and decode events
  into named fields behind `stellar.Decoder`.
- Dashboard improvements (kept deliberately minimal in the MVP).

## Pull requests

- Keep PRs focused; separate refactors from features.
- Describe the behavior change and how you tested it.
- New extension points should come with a short doc comment telling the next
  contributor how to use them.
