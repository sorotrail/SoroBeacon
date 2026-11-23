# GraphQL API

SoroBeacon serves a GraphQL endpoint at `/graphql`. The schema is defined in
`internal/api/graphql/schema.graphqls` and the resolvers are in
`internal/api/graphql/resolver.go`.

{% hint style="warning" %}
**The endpoint does not execute queries yet.** Every request returns the same
fixed response, whatever it asks for:

```sh
curl -s -X POST localhost:8080/graphql \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ monitors { edges { node { id name } } } }"}'
# {"data":{"__schema":{"queryType":{"name":"Query"}}}}
```

A mutation, an introspection query, a GET and a malformed body all get the same
body back. The handler is wired to the resolver but never calls it — verified
against a running instance. **Use the [HTTP API](api.md) or the gRPC API
today**; this page documents the schema that is defined,
so a client can be written against it before the executor lands, plus what the
endpoint actually does.

Two further facts, both verified against a running instance with `API_TOKEN`
set, and both reported as issues:

* **`/graphql` is not authenticated.** It is mounted on the root router, above
  the API's auth middleware, so it answers with `200` while `/api/v1` answers
  `401` on the same instance. Nothing is exposed today because no query runs,
  but the route has to move behind auth before the executor is wired up.
* **The playground at `/graphql/playground` is likewise unauthenticated.**
{% endhint %}

## Connecting

`POST /graphql` (any method is accepted; `POST` with
`Content-Type: application/json` is the convention), on the same HTTP port as
the REST API. Set `GRAPHQL_PLAYGROUND=true` for the playground at
`/graphql/playground`. `GRAPHQL_MAX_DEPTH` (default `10`, valid 1–50) and
`GRAPHQL_MAX_COMPLEXITY` (default `1000`, valid 1–10000) are read at startup.

```sh
curl -s -X POST localhost:8080/graphql \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $API_TOKEN" \
  -d '{"query":"{ stats { monitors alerts } }","variables":{}}'
```

## Introspection and query limits

**Introspection is not available.** There is no executor and therefore no
introspection response; the `{"__schema":{"queryType":{"name":"Query"}}}` body
above is a literal, not a schema dump. To explore the schema, read
`schema.graphqls` — it is the authoritative list of every field below, and it
is the file to point a code generator at.

**There is no depth or complexity limit today.** `GRAPHQL_MAX_DEPTH` and
`GRAPHQL_MAX_COMPLEXITY` are parsed and validated at startup (a value outside
the allowed range fails the boot, which is why they are documented at all),
and both are logged in the startup summary — but the handler never consults
them, so a query of any depth would be accepted if one were ever executed.
Nothing will reject an over-large query.

## The schema

Read-only. There is **no `Mutation` type and no `Subscription` type**: the
schema declares one root type, `Query`, and every write goes through the
[HTTP API](api.md).

### Queries

| Field | Arguments | Returns | What it does |
| --- | --- | --- | --- |
| `monitor` | `id: ID!` | `Monitor` | One monitor by numeric id. |
| `monitors` | `enabled`, `limit`, `cursor`, `query`, `sort` | `MonitorConnection!` | A page of monitors, filtered and sorted. |
| `alert` | `id: ID!` | `Alert` | One alert by numeric id. |
| `alerts` | `monitorId`, `ruleId`, `contractId`, `from`, `to`, `severity`, `limit`, `cursor`, `sort` | `AlertConnection!` | A page of alerts. |
| `stats` | — | `Stats!` | Instance-wide counts and the poller's last known ledger. |
| `alertSeries` | `days` | `[AlertDayCount!]!` | Alerts per UTC day, for a chart. |

Arguments, and what they actually do — several differ from what the name
suggests:

| Argument | Applies to | Behaviour |
| --- | --- | --- |
| `limit` | `monitors` | 1–500 honoured; anything ≤ 0 or above 500 becomes **50**. Silently — a clamped page is indistinguishable from a requested one. |
| `limit` | `alerts` | 1–1000 honoured; anything ≤ 0 or above 1000 becomes **100**. Note the different cap from `monitors`. |
| `cursor` | `monitors`, `alerts` | An opaque string carrying the id of the last row of the previous page. A cursor that is not a number is **ignored**, not rejected: you get page 1 again rather than an error. |
| `enabled` | `monitors` | Tri-state. Omitted returns both; `true` and `false` filter. |
| `query` | `monitors` | Case-insensitive substring of the monitor's name. |
| `sort` | `monitors` | `NAME`, `ID` or `CREATED_AT` (see [enums](#enums)). Omitted means id-descending. |
| `sort` | `alerts` | `CREATED_AT_DESC` (default) or `CREATED_AT_ASC`. |
| `contractId` | `alerts` | Matches `payload.contract_id`. |
| `from` / `to` | `alerts` | `DateTime`, RFC 3339. `to` is exclusive. |
| `severity` | `alerts` | Minimum severity: `INFO`, `WARNING` or `CRITICAL`. |
| `days` | `alertSeries` | Defaults to 30; above 90 it is clamped to 90. |

### Types

**`Monitor`** — one watched contract set.

| Field | Type | Meaning |
| --- | --- | --- |
| `id` | `ID!` | Numeric id, serialised as a string. |
| `name` | `String!` | Display name. |
| `contractIds` | `[String!]!` | Soroban contract addresses watched. Only meaningful together with `network` on a multi-network instance — a contract id identifies an address on one chain — and this schema has no field for either. Use REST there. |
| `enabled` | `Boolean!` | Whether the poller is watching it. |
| `priority` | `Priority!` | Poll order within a cycle: `LOW`, `NORMAL` (the default) or `HIGH`. |
| `createdAt` | `DateTime!` | When the monitor was created. |
| `lastMatchedAt` | `DateTime` | When it last produced an alert. Null means never — do not substitute a fake timestamp. |
| `rules` | `[Rule!]!` | The monitor's rules. **Costs one store query per monitor**; see [Cost of nested lists](#cost-of-nested-lists). |
| `channels` | `[Channel!]!` | The channels it alerts to. Same cost. |

**`Rule`** — one condition evaluated against every event of its monitor's
contracts.

| Field | Type | Meaning |
| --- | --- | --- |
| `id` / `monitorId` | `ID!` | The rule and its parent monitor. |
| `type` | `String!` | Which evaluator runs, e.g. `event_emitted`. |
| `params` | `JSON!` | The evaluator's parameters, verbatim. See the [params reference](../rules/params.md) — this schema does not validate them. |
| `enabled` | `Boolean!` | Whether it is evaluated. |
| `severity` | `Severity!` | Severity of the alerts it raises. |

**`Channel`** — a notification destination.

| Field | Type | Meaning |
| --- | --- | --- |
| `id` / `name` / `type` | `ID!` / `String!` / `String!` | Identity and which notifier it is. |
| `enabled` | `Boolean!` | Whether it is in rotation. |
| `digestMode` | `String` | `"window"` or null. See [Digest mode](../channels/digest.md). |
| `digestWindowSeconds` | `Int` | Digest window length; 0 when not digesting. |
| `minSeverity` | `Severity` | The lowest severity this channel accepts. |
| `createdAt` | `DateTime!` | When it was created. |

**Channel configuration is not resolvable.** There is no `config` field on this
type, and the resolver's conversion deliberately omits it — webhook URLs, bot
tokens and SMTP passwords are never returned over GraphQL, exactly as
[the REST API never returns them](api.md#channels). Nor are the channel's
delivery-health fields.

**`Alert`** — one rule match.

| Field | Type | Meaning |
| --- | --- | --- |
| `id` / `monitorId` / `ruleId` | `ID!` | What fired, where. |
| `eventId` | `String!` | The source event's id. |
| `payload` | `JSON!` | The raw event payload. Where every rule-specific field lives — including `contract_id`, which is why there is no `contractId` field on the type. |
| `createdAt` | `DateTime!` | When it was stored. |
| `severity` | `Severity!` | `INFO`, `WARNING` or `CRITICAL`. |
| `ledger` | `Int` | The ledger the event closed in. |
| `retractedAt` | `DateTime` | Set when a reorg orphaned the event. **Check it**: a retracted alert is not something you should act on. |
| `backfilled` | `Boolean!` | True when the alert was reconstructed from history rather than seen live. |
| `inhibitedByRuleId` | `ID` | The cooldown rule that suppressed this alert, if any. |

**`Stats`** and **`AlertDayCount`**

| Type | Field | Type | Meaning |
| --- | --- | --- | --- |
| `Stats` | `monitors` / `rules` / `channels` / `alerts` | `Int!` | Instance-wide totals. |
| | `alertsLast24h` | `Int!` | Same, over the last 24 hours. |
| | `lastLedger` | `Int!` | Last ingested ledger. |
| | `lastPollAt` | `DateTime` | Null if the poller has not completed a cycle. |
| `AlertDayCount` | `day` | `String!` | A UTC calendar date, `YYYY-MM-DD`. |
| | `count` | `Int!` | Alerts that day. Quiet days appear as explicit zeroes. |

**Connection types** — `MonitorConnection` / `AlertConnection` are the same
shape: `edges` (each with `node` and a `cursor`) plus `pageInfo`.

| `PageInfo` field | Type | Meaning |
| --- | --- | --- |
| `hasNextPage` | `Boolean!` | True when the page came back full. **A heuristic**: a page of exactly `limit` rows with nothing after it still reports true, so expect one empty final page rather than a false. |
| `hasPreviousPage` | `Boolean!` | Always false — paging forward only. |
| `startCursor` / `endCursor` | `String` | Ids of the first and last row on this page. The resolvers always populate these, so an empty page yields `""` rather than `null` despite the field being nullable. |

#### Enums

| Enum | Values |
| --- | --- |
| `Priority` | `LOW`, `NORMAL`, `HIGH` |
| `Severity` | `INFO`, `WARNING`, `CRITICAL` |
| `MonitorSort` | `NAME`, `ID`, `CREATED_AT` |
| `AlertSort` | `CREATED_AT_DESC`, `CREATED_AT_ASC` |

#### Scalars

`DateTime` is an RFC 3339 string. `JSON` is the raw decoded value — an object,
array or scalar, not a string. Both are emitted as-is and must be parsed as
their underlying JSON type, not coerced.

## Errors and partial data

A resolver that fails returns a GraphQL error, not an exception: the response
carries an `errors` array and the field is null. Which fields are null and
which are absent is the schema's nullability, and it is worth reading rather
than assuming:

* **`monitor` and `alert` return `null` for a missing row, with no error.** A
  non-numeric `id` *is* an error (`invalid monitor ID: …`). So `{ monitor(id: "999") }`
  succeeds with `data.monitor = null` — check for null rather than treating a
  successful response as a hit.
* Store failures are errors: `failed to list monitors: …`, `failed to get stats: …`.
  The message carries the underlying store error, so a client sees the same
  text a REST client would.
* Because `Alert.inhibitedByRuleId` is nullable and the parent `Rule` lookup
  can fail independently, a query that resolves it can return partial data with
  errors alongside.
* Nothing in the schema marks a nested list as optional, so a resolver failure
  on `Monitor.rules` nulls that field's parent chain under normal GraphQL
  propagation rather than degrading quietly.

## Cost of nested lists

There is **no batching** in the resolver package — no DataLoader, and no
per-request cache of any kind. Each nested list field is one store query per
parent row:

* `monitors { edges { node { rules { … } } } }` runs **one rule query per
  monitor on the page**, and `channels` the same again. A 50-row page asking
  for both is 101 queries.
* `alerts { … inhibitedByRuleId }` is resolved by fetching the inhibiting rule
  for **each alert** that has one.

With the default `limit` of 50 that is fine. A client that raises `limit` and
asks for `rules` at the same time multiplies: `limit: 200` with `rules` is 201
queries. Ask for the flat fields, and use a second, narrower query for the
rules of the one monitor you care about.

## Relationship to the REST and gRPC APIs

Three APIs, and a reason to prefer each:

| | REST | GraphQL | gRPC |
| --- | --- | --- | --- |
| Complete surface | yes | no — read-only, and not executed today | no — monitors and alerts only |
| Writes | yes | none | none, by design |
| Shape | fixed endpoints, one response each | composed per request | typed messages |
| Cost of a nested field | one endpoint per shape | one query per parent row (above) | flat messages |
| Works today | yes | returns a fixed response | unary methods fail to decode; see the gRPC reference |

**Use REST.** It is the only complete surface, it enforces token scopes, and it
is what the dashboard itself runs on. Reach for GraphQL only for a
read-only client that would otherwise need several round trips — and note the
N+1 cost above before writing one.

## A worked query

The shape a client would write, and what the response looks like once an
executor exists. Every field is one the schema declares:

```graphql
query MonitorDetail($id: ID!) {
  monitor(id: $id) {
    id
    name
    enabled
    priority
    createdAt
    lastMatchedAt
    contractIds
    rules { id type severity enabled params }
    channels { id name type enabled minSeverity }
  }
  alerts(monitorId: $id, severity: CRITICAL, limit: 20) {
    edges {
      cursor
      node { id createdAt severity retractedAt payload }
    }
    pageInfo { hasNextPage endCursor }
  }
}
```

```json
{
  "data": {
    "monitor": {
      "id": "1",
      "name": "Payment events",
      "enabled": true,
      "priority": "NORMAL",
      "createdAt": "2026-10-19T09:35:04Z",
      "lastMatchedAt": null,
      "contractIds": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
      "rules": [
        { "id": "1", "type": "event_emitted", "severity": "WARNING", "enabled": true,
          "params": { "event_name": "transfer" } }
      ],
      "channels": [
        { "id": "1", "name": "ops-alerts", "type": "slack", "enabled": true, "minSeverity": null }
      ]
    },
    "alerts": {
      "edges": [],
      "pageInfo": { "hasNextPage": false, "endCursor": "" }
    }
  }
}
```

Note `lastMatchedAt: null` for a monitor that has never fired, and
`endCursor: ""` rather than `null` on the empty page — both are the resolver
behaviour described above, not typos. Note also what is absent: no channel
`config`, and no channel health.

## Findings

Reported as issues rather than documented around:

1. **The handler is a stub.** `NewHandler` mounts a closure that writes a fixed
   JSON body and never touches the resolver it was given, so no query, mutation
   or introspection query can run. `internal/api/graphql` has no tests, which is
   why this has gone unnoticed.
2. **`/graphql` and `/graphql/playground` are unauthenticated.** Both are mounted
   on the root router, above the API's auth middleware — verified: with
   `API_TOKEN` set, `/api/v1/monitors` returns `401` and `/graphql` returns
   `200` on the same instance.
3. **`GRAPHQL_MAX_DEPTH` and `GRAPHQL_MAX_COMPLEXITY` are never enforced.** They
   are parsed, range-checked and logged, and the handler ignores them.
4. **The workspace in the request context is not used.** As on the gRPC path,
   nothing puts a workspace into the context, so store queries resolve to the
   default workspace on a multi-workspace instance.
5. **The resolver package has no tests at all**, so every behaviour documented
   above as intended is currently unverified by anything but this reading.

## Persisted Operations and Allowlist Mode

For multi-tenant defense and performance, SoroBeacon supports persisted GraphQL operations. Rather than accepting arbitrary caller-composed documents, clients register query documents and invoke them by identifier (SHA-256 hash).

### How Invocation Works

A client invokes an operation by sending its SHA-256 hash in the JSON body:

```json
{
  "id": "a591a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f146e",
  "variables": { "limit": 20 }
}
```

Or via standard Apollo Automatic Persisted Query (APQ) extensions:

```json
{
  "extensions": {
    "persistedQuery": {
      "version": 1,
      "sha256Hash": "a591a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f146e"
    }
  },
  "variables": { "limit": 20 }
}
```

If an identifier is unknown, the server responds distinguishably:

```json
{
  "errors": [
    {
      "message": "PersistedQueryNotFound",
      "extensions": {
        "code": "PERSISTED_QUERY_NOT_FOUND"
      }
    }
  ]
}
```

### Allowlist-Only Mode (`GRAPHQL_ALLOWLIST_ONLY=true`)

By default (`GRAPHQL_ALLOWLIST_ONLY=false`), open queries are accepted. When allowlist mode is enabled (`GRAPHQL_ALLOWLIST_ONLY=true`), arbitrary ad-hoc queries are rejected with `PERSISTED_QUERY_NOT_ALLOWED` unless the document text matches a registered persisted operation's canonical hash.

### Privileged Registration API

Persisted operations are stored in the database (`persisted_operations` table) across PostgreSQL and SQLite, surviving process restarts and synchronizing across replicas.

Registration is an authenticated, administrative operation under `/api/v1/graphql/operations` requiring `API_TOKEN` or `WORKSPACE_TOKENS` with `tokens:write` / admin privilege. Automatic client registration on cache miss is intentionally prohibited to prevent bypasses:

* `POST /api/v1/graphql/operations`: Register an operation. Requires JSON `{"name": "...", "document": "..."}`. Normalizes document whitespace and verifies the SHA-256 hash. Returns `201 Created`.
* `GET /api/v1/graphql/operations`: List registered operations (reviewable audit surface).
* `GET /api/v1/graphql/operations/{id}`: Inspect a registered operation.
* `DELETE /api/v1/graphql/operations/{id}`: Delete an operation from the allowlist.

### Cost and Complexity Limits on Persisted Queries

A registered query is vetted, not exempt. `GRAPHQL_MAX_DEPTH` (depth ceiling) and `GRAPHQL_MAX_COMPLEXITY` (complexity ceiling) are strictly enforced against persisted operations. Variable arguments (such as large `limit` inputs) multiply complexity scores and will trigger `QUERY_COMPLEXITY_EXCEEDED` or `QUERY_DEPTH_EXCEEDED` rejections when exceeded.

### Global vs Per-Workspace Operation Tenancy

The GraphQL schema defines shared structural types (monitors, alerts, rules, channels, stats). Multi-tenant isolation is enforced at execution time against the requesting caller's authenticated workspace context, not by partitioning query syntax. Operation shapes are therefore registered globally in `persisted_operations`, allowing first-party dashboard queries and multi-tenant clients to share vetted shapes without per-workspace duplicate registration.