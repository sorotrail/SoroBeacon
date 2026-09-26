# A code tour: following one event

This is a map, not a manual — the design rationale lives in
[Architecture](../reference/architecture.md) and the how-to in
[Extending SoroBeacon](extending.md). What follows is one event, from the
chain to a notification, in the order the code actually runs it. Every
function named below is real; open each file as you go and the tour should
read top to bottom without surprises.

Setup first if you haven't already: [Development guide](development.md).

## The wiring: `cmd/sorobeacon/main.go`

Everything is connected in one place, `run` in `cmd/sorobeacon/main.go`.
Reading it top to bottom shows the whole pipeline as a set of interfaces:

* the **event source** — `poller.NewRPCSource(rpc, decoder)` in `rpc` mode,
  or `sorotrail.NewSource(stc)` in upstream mode. Either way it is just a
  `poller.EventSource` (see `internal/poller/source.go`): `LatestLedger` plus
  a paged `FetchEvents`. This is the single seam between the poller and
  wherever events come from.
* the **decoder** — `stellar.NewSpecDecoder(stellar.DefaultDecoder{}, ...)`
  wraps the plain decoder with contract-spec-aware named fields.
* the **rules registry** — `rules.NewRegistry()` with the seven built-in rule
  types registered.
* the **dispatcher** — `notify.NewDispatcher(st, factory, log)`.
* the **poller** — `poller.New(src, st, registry, dispatcher, ...)` then
  `go p.Run(ctx)`.

The same registry and factory are handed to the API server and the dashboard,
so a rule type or channel registered here shows up everywhere at once.

## Stop 1 — the poller fetches (`internal/poller`)

One ingest cycle is `Poller.Poll` (`internal/poller/poller.go`); `Poller.Run`
is only the timing loop around it. In order it:

1. `ListMonitors(ctx, true)` for the enabled monitors and maps each contract
   ID to the monitors watching it, keeping the highest priority per contract.
2. Builds a `[]Watch` (one per contract, `internal/poller/source.go`) with a
   server-side topic filter derived from the rules' event names — an
   optimisation, never the source of truth.
3. Orders the watch list with `Scheduler.Order` (`internal/poller/schedule.go`):
   high-priority contracts first in a weighted round-robin that never starves
   the low tier.
4. Reads the checkpoint with `GetIngestState`; a zero `LastLedger` is a cold
   start at the source's tip (`LatestLedger`), otherwise it resumes at
   `LastLedger + 1`.
5. Runs `detectReorg` (`internal/poller/reorg.go`), which re-reads recent
   ledger hashes and, on a changed hash, rewinds the checkpoint and calls
   `RetractAlertsFromLedger` on the store — orphaned alerts are flagged, never
   deleted.
6. Pages `FetchEvents` until the returned `NextCursor` is empty. The cursor is
   opaque: the default source, `RPCSource` (`internal/poller/rpcsource.go`),
   encodes its batch position in it because the RPC caps getEvents at 5
   filters × 5 contract IDs. Events arrive already decoded.

Each event then goes to `Poller.handleEvent`, which loops over the monitors
watching its contract and evaluates every enabled rule via
`Registry.Evaluate`, with the rule id in the context
(`rules.WithRuleID`) so a stateful evaluator can key its state. A match calls
`Poller.fireAlert`, which builds the alert payload and hands it to the store.
At the end of the cycle the checkpoint advances to the minimum `latestLedger`
the source reported.

## Stop 2 — decoding (`internal/stellar`)

Sources own decoding, so the poller never sees raw XDR. The contract is the
`Decoder` interface (`internal/stellar/decoder.go`):
`DefaultDecoder.DecodeEvent` prefers the RPC's `xdrFormat: "json"` output and
falls back to base64 XDR ScVals decoded via the Stellar SDK. Everything
normalises into one small value vocabulary —

`nil`, `bool`, `string`, `*big.Int`, `[]byte`, `[]any`, `map[string]any`

— defined on `DecodedEvent` (`internal/stellar/types.go`, whose `EventName`
reads the conventional first topic). `SpecDecoder`
(`internal/stellar/spec_decoder.go`) wraps any decoder and additionally fills
`DecodedEvent.Fields` from the contract's SEP-0048 spec, fetched lazily and
cached per contract through `NewRPCSpecSource`
(`internal/stellar/spec_source.go`).

The helper functions every rule builds on are in
`internal/stellar/values.go`: `Canon` (canonical string rendering),
`ToBigFloat` (numeric coercion) and `Lookup` (dot-path resolution).

## Stop 3 — rule evaluation (`internal/rules`)

A rule type is anything that satisfies `rules.RuleEvaluator`
(`internal/rules/rules.go`): `Evaluate` on one event, `Validate` on the raw
JSON params (so the API can reject bad rules at create time). The poller
resolves the rule's `Type` through `Registry.Evaluate` against the registry
built in `NewRegistry`.

The built-ins, one file each:

| Type | File |
| --- | --- |
| `event_emitted` | `internal/rules/event_emitted.go` |
| `value_threshold` | `internal/rules/value_threshold.go` |
| `token_event` | `internal/rules/token_event.go` |
| `frequency_threshold` | `internal/rules/frequency_threshold.go` |
| `topic_regex` | `internal/rules/topic_regex.go` |
| `topic_position` | `internal/rules/topic_position.go` |
| `address_watchlist` | `internal/rules/address_watchlist.go` |

`frequency_threshold` is the interesting one: it is stateful, keeps a rolling
window keyed by `rules.RuleID(ctx)`, and chooses the event id its alert is
stored under via `AlertEventIDer`. The optional cooldown (`internal/rules/cooldown.go`)
is not an evaluator at all — it is read from the rule's params and enforced
in the store.

For each stop in the tour there is a test next door (`poller_test.go`,
`decoder_test.go`, `rules_test.go`, …); the conformance thinking is the same
everywhere — break something and the unit test should fail.

## Stop 4 — alert persistence (`internal/store`)

The store is a set of narrow interfaces in `internal/store/store.go`
(`Monitors`, `Rules`, `Channels`, `Alerts`, `Ingest`, …); the poller declares
exactly the slice it needs in its own `Store` interface
(`internal/poller/poller.go`). Postgres (`internal/store/postgres.go`) and
SQLite (`internal/store/sqlite.go`) both implement them, sharing one
behavioural conformance suite, `internal/store/conformance_test.go`.

The alert lands in `CreateAlert` (Postgres line-of-truth:
`internal/store/postgres.go`), which enforces two gates inside one
transaction:

* the **dedup guard** — `ON CONFLICT (rule_id, event_id) DO NOTHING`, so a
  rule can never fire twice for the same event, even across restarts and
  replays (`AlertDuplicate`);
* the **cooldown** — a match inside the rule's cooldown window is counted but
  not delivered (`AlertSuppressed`).

Delivery attempts are recorded with `RecordDeliveryAttempt`, read back with
`ListDeliveryAttempts`, and the poller's checkpoint lives in `ingest_state`
via `GetIngestState` / `SetIngestState`. On Postgres, `alerts` is
range-partitioned by month (`internal/store/partition.go`); retention drops
whole expired partitions or, when `ARCHIVE_URL` is set, archives each batch
first (`internal/store/prune.go`).

## Stop 5 — delivery (`internal/notify`)

`Poller.fireAlert` hands the created alert to `Dispatcher.Dispatch`
(`internal/notify/dispatcher.go`), which looks up the monitor's enabled
channels (`ListChannelsForMonitor`), builds each notifier through the
`Factory` (`internal/notify/notify.go`) and delivers — up to three attempts
with exponential backoff — recording every attempt, success or failure, in
`delivery_attempts`. One misbehaving channel never blocks the others or the
poller.

A channel is anything satisfying `notify.Notifier` (`Send(ctx, a Alert)`),
built by a constructor registered in `DefaultFactory`. The seven built-ins
are one small file each (`slack.go` is the smallest complete example;
`webhook.go` shows the HMAC signing pattern). Secrets live in each channel's
config JSON: never log them, never return them from the API, and never put
them in error messages — errors become delivery `response_snippet`s.

## Stop 6 — reading it back (`internal/api`, `internal/web`)

The JSON API is a chi router built by `Server.Routes`
(`internal/api/api.go`), mounted under `/api/v1` in `cmd/sorobeacon`. For the
event you followed: `GET /api/v1/alerts` → `Server.listAlerts`
(`internal/api/alerts.go`, keyset pagination with `next_cursor`), and
`GET /api/v1/alerts/{id}/deliveries` → `Server.listDeliveries` for the
attempt history. Rules and channels are created through the same registry and
factory the pipeline uses, so `POST /api/v1/monitors/1/rules` calls
`Validate` on the evaluator and rejects bad params before anything is stored.

The dashboard (`internal/web/web.go`) is server-rendered
`html/template` + htmx over the same store: the monitors list, the alert
history (`Server.alertDetail` renders one alert and its deliveries), and a
rule builder driven by the evaluators' param schemas
(`internal/web/rulebuilder.go`). Auth for both surfaces lives in
`internal/auth`; request correlation in `internal/reqid`; instrumentation in
`internal/metrics`.

## Where to extend

Two seams cover most contributions, and both already have worked examples in
[Extending SoroBeacon](extending.md):

* **A rule type** — implement `rules.RuleEvaluator` and register it in
  `NewRegistry` (`internal/rules/rules.go`); the poller, API validation and
  rule builder pick it up with no further wiring.
* **A notification channel** — implement `notify.Notifier`, validate config
  in its constructor, and register it in `DefaultFactory`
  (`internal/notify/notify.go`).

A third, narrower seam: **another event source** is an implementation of
`poller.EventSource` (`internal/poller/source.go`) plus one case in the mode
switch in `cmd/sorobeacon/main.go` — nothing in the poller changes.

## Before you open the PR

* Add or extend a unit test next to the code you touched; see
  [Development guide](development.md) for the store conformance suite.
* Run the checks CI runs: `go build ./... && go vet ./... && go test ./...`
  and `golangci-lint run`.
* One request across the codebase: never log, return or echo channel config
  contents — they hold webhook URLs, bot tokens and SMTP credentials, and
  that includes error messages and delivery snippets.
