# Health and readiness endpoints

SoroBeacon serves three health endpoints for orchestrators, load balancers
and operators. They live under `/api/v1` on the same listener as the API and
the dashboard (`HTTP_ADDR`), always answer JSON, and every response carries
an `X-Request-ID`.

| Endpoint | Answers | Success | Failure |
| --- | --- | --- | --- |
| `GET /api/v1/livez` | Is the process up? | always `200` | never |
| `GET /api/v1/readyz` | Can this instance do useful work right now? | `200` with `status: "ready"` | `503` with `status: "not ready"` when any check fails |
| `GET /api/v1/health` | Human-readable summary | `200` `status: "ok"` | `503` `status: "degraded"` when the database or the RPC is unreachable |

The failure conditions below are taken from `internal/api/probes.go`
(`livez`, `readyz`) and `internal/api/api.go` (`health`), and were confirmed
against a running instance — including deliberately breaking readiness.

## livez — liveness

`GET /api/v1/livez` checks **nothing**: it always answers
`{"status":"alive"}` with `200`. A process whose database and RPC are both
down is still alive, and a liveness probe that failed on dependencies would
get the container restarted in a loop instead of merely taken out of
rotation.

Wire it to the orchestrator's **liveness probe** and to nothing else.

## readyz — readiness

`GET /api/v1/readyz` runs its checks concurrently, each bounded by a 3-second
per-check timeout (`probeTimeout`), so one hanging dependency cannot hang the
probe. Exactly what makes it fail:

| Check | Healthy when | Detail on failure |
| --- | --- | --- |
| `database` | `store.Ping` succeeds | the ping error text |
| `rpc` | the event source answers `getHealth` | the RPC error text; on success the detail is `latest ledger N` |
| `poller` | only present when `READYZ_LAG_THRESHOLD` is set | `ledger lag N exceeds threshold T` |

Any unhealthy check makes the whole endpoint answer `503` with
`"status":"not ready"`. One failing dependency still says which one:

```json
{
  "checks": {
    "database": {"healthy": true},
    "rpc": {"healthy": false, "detail": "getHealth: Post \"http://127.0.0.1:9/no-such-rpc\": dial tcp 127.0.0.1:9: connect: connection refused"}
  },
  "status": "not ready"
}
```

(Verified by pointing a running instance at an unreachable RPC.)

### Why liveness and readiness are different here

Readiness failing means *route traffic elsewhere*; the process may be
perfectly fine — the RPC node may be down, the database may be restarting,
the poller may be behind. Restarting the container on a readiness failure
would be wrong twice over: it would destroy a healthy process for a
dependency's problem, and the restarted instance would face the same
unhealthy dependency — that is how restart loops are born. Liveness failing
(genuinely never, in this design) is the only thing that should ever restart
the container.

### The poller-lag check

`READYZ_LAG_THRESHOLD` (default `0`, disabled) adds a third check: it fails
`/readyz` when `ledger lag` — the chain tip minus the last ledger the poller
processed — exceeds the threshold.

Why it is off by default: lag is the one check that can fail transiently
under perfectly normal conditions. A poll interval of 5s against ~5s ledger
close means an ordinary cycle can trail by a couple of ledgers, and a slow
`getEvents` page-through can trail further without anything being wrong.
A threshold set too tightly would flap readiness on a healthy instance —
and, per the section above, readiness failure should mean traffic removal,
not restarts.

When it is worth setting: you run more than one SoroBeacon replica behind a
load balancer and want to stop routing to a replica whose ingestion has
stalled, rather than just observing it on the dashboard.

Choosing a value, from the metrics the check reports:

1. Watch `sorobeacon_poll_lag` on `/metrics` (or `ledger_lag` on
   `/api/v1/health`) for a few days under normal load, including backlog
   recovery after downtime.
2. Set the threshold comfortably above the worst steady-state lag you saw.
3. Leave headroom for restarts: a fresh instance has not polled yet, and the
   check is deliberately **lenient there** — until the first successful poll
   completes, the poller check reports `"waiting for first poll"` and stays
   healthy, so a cold start cannot flap readiness. Lag only counts after the
   poller has proven it can make progress.

A multiple of the poll interval is a sane floor: with the default
`POLL_INTERVAL=5s`, lag is measured in ledgers, so anything below the number
of ledgers closed in one or two poll cycles is almost certainly too tight.

## health — the operator's summary

`GET /api/v1/health` is the oldest of the three and the one the shipped
`docker-compose.yml` uses for its container healthcheck. It pings the
database and calls `getHealth` on the RPC with a 5-second budget and answers
`status: "degraded"` with `503` when either fails; on success it includes
`rpc_latest_ledger`, and once a poll has completed it adds
`last_processed_ledger`, `latest_chain_ledger`, `ledger_lag` and
`last_successful_poll`. It is a good `curl`-from-your-laptop endpoint; for
orchestration prefer the pair above, whose semantics are stricter.

## Authentication

The three endpoints are **exempt from `API_TOKEN` authentication and from
rate limiting** (`isProbePath` in `internal/api/ratelimit.go`), so an
authenticated deployment cannot fail its own health checks and the compose
healthcheck works with no token in the container. The price is that the
readiness detail — dependency names and error strings — is readable without
a credential. Keep the listener off the public internet; the same applies to
`/metrics`.

## Probe configuration

Kubernetes, ready to paste:

```yaml
livenessProbe:
  httpGet:
    path: /api/v1/livez
    port: 8080
  periodSeconds: 10
  timeoutSeconds: 2
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /api/v1/readyz
    port: 8080
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 3

startupProbe:
  httpGet:
    path: /api/v1/readyz
    port: 8080
  periodSeconds: 5
  timeoutSeconds: 5
  failureThreshold: 24   # up to 2 minutes for migrations and first poll
```

Timeout reasoning:

* **Liveness timeout 2s** — the handler touches no dependency and does no
  work; if it cannot answer in 2 seconds the process is genuinely wedged and
  a restart is warranted.
* **Readiness timeout 5s** — above the server's own 3-second per-check
  bound, so a dependency timing out is reported by SoroBeacon as a clean
  `503` (a *readiness* failure, traffic removed) rather than by the
  orchestrator as a probe timeout. Reaching the endpoint must never be the
  bottleneck.
* **failureThreshold 3 / periodSeconds 10** — roughly 30 seconds of
  sustained failure before acting on it, enough to ride out a single slow
  poll cycle, a database connection blip or a testnet RPC hiccup without
  flapping.
* **startupProbe** — SoroBeacon applies migrations at boot and the poller
  needs one cycle before readiness is meaningful; the startup probe covers
  that window so neither probe acts during it. 24 × 5s = 2 minutes of
  grace, generous for the embedded migrations; raise it for very large
  databases on slow storage.

Docker / Docker Compose, matching the shape already in
`docker-compose.yml` (it uses `/api/v1/health`, which is why the compose
healthcheck works with `API_TOKEN` set):

```yaml
healthcheck:
  test: ["CMD", "wget", "-q", "-O-", "http://localhost:8080/api/v1/readyz"]
  interval: 10s
  timeout: 5s        # above the server's own 3s per-check bound
  retries: 3
  start_period: 30s  # migrations + first poll
```

A load balancer only needs the one endpoint: `GET /api/v1/readyz`, expect
`200`, remove the target on anything else. It never needs `API_TOKEN`.

## Verifying it yourself

Healthy path and failure path against a running instance, including the
deliberate readiness failure this page was written from:

```sh
# Failure path: an unreachable RPC must make readyz (and health) fail.
DATABASE_URL=sqlite:///tmp/sb.db HTTP_ADDR=127.0.0.1:18099 \
  RPC_URL=http://127.0.0.1:9/no-such-rpc ./bin/sorobeacon &
curl -s -w '\n%{http_code}\n' localhost:18099/api/v1/readyz
# → {"checks":{"database":{"healthy":true},"rpc":{"healthy":false,...}},
#    "status":"not ready"}
# → 503

# Healthy path: a reachable RPC, every check green.
DATABASE_URL=sqlite:///tmp/sb2.db HTTP_ADDR=127.0.0.1:18100 \
  RPC_URL=https://soroban-testnet.stellar.org ./bin/sorobeacon &
curl -s -w '\n%{http_code}\n' localhost:18100/api/v1/readyz
# → {"checks":{"database":{"healthy":true},"poller":...,
#    "rpc":{"healthy":true,"detail":"latest ledger 4850161"}},
#    "status":"ready"}
# → 200
```

The lag-failure branch is additionally pinned by
`TestReadyzFailsWhenLagExceedsThreshold` in `internal/api/probes_test.go`,
which drives a stubbed poller past the threshold and asserts the `503` and
the unhealthy `poller` check.
