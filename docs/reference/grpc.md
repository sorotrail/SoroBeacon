# gRPC API

SoroBeacon serves a gRPC API alongside the [HTTP API](api.md): one read-only
service, `sorobeacon.v1.MonitorService`, with three unary methods and one
server-streaming subscription. The write surface is deliberately REST-only, so
there is one place that validates mutations — see
[Relationship to the REST API](#relationship-to-the-rest-api).

{% hint style="warning" %}
**The three unary methods do not currently work.** Every call fails before it
reaches a handler:

```
Code: Internal
Message: grpc: failed to unmarshal the received message: failed to unmarshal,
        message is *grpc.ListMonitorsRequest, want proto.Message
```

`internal/grpc` registers its service by hand, with Go structs that are not
generated protobuf messages, so the codec cannot decode a request into them.
`StreamAlerts` is the one method whose authentication interceptor runs before
that decode, which is why it answers `Unauthenticated` rather than `Internal`.
Both problems are reported as issues rather than worked around here; until they
are fixed, use the [HTTP API](api.md).

This page documents the intended contract — what the `.proto` in this
repository declares, which is what a client generated from it will expect —
plus the behaviour that is observable today, stated per method.
{% endhint %}

## Connecting

gRPC is **off by default**. The server binds no gRPC port at all unless
`GRPC_ADDR` is set, so an existing deployment does not open a new port
without opting in:

```sh
GRPC_ADDR=:9090 ./sorobeacon
```

The address is a plain TCP listen address, and the transport is **HTTP/2
cleartext (h2c)**. There is no TLS and no mTLS option — the server is built
with no transport credentials at all — so anything reaching the port can read
the data on it in the clear. Keep it on a trusted network or terminate TLS in
front of it; do not publish the port.

Clients therefore need cleartext enabled. With
[grpcurl](https://github.com/fullstorydev/grpcurl):

```sh
grpcurl -plaintext -import-path proto -proto sorobeacon/v1/sorobeacon.proto \
  -d '{"id": 1}' 127.0.0.1:9090 sorobeacon.v1.MonitorService/GetMonitor
```

A Go client dials it the same way:

```go
conn, err := grpc.NewClient("127.0.0.1:9090", grpc.WithTransportCredentials(insecure.NewCredentials()))
```

## Authentication

The token is the same credential the HTTP API uses, sent as gRPC metadata:

| | |
| --- | --- |
| Metadata key | `authorization` (lowercase — gRPC lowercases header names) |
| Value | `Bearer <token>` |

```sh
grpcurl -plaintext -H "authorization: Bearer $API_TOKEN" \
  -import-path proto -proto sorobeacon/v1/sorobeacon.proto \
  -d '{}' 127.0.0.1:9090 sorobeacon.v1.MonitorService/StreamAlerts
```

A unary interceptor and a stream interceptor both run before the handler and
share one check. With `API_TOKEN` unset the check is a no-op and every call is
allowed, exactly as on the REST API.

Two differences from REST are worth knowing before you design around them:

* **Scoped tokens do not authenticate here.** A token minted by `POST /tokens`
  (the `sb_` prefix) is refused with `Unauthenticated: invalid token`. The
  gRPC path calls `Authenticator.Verify`, which consults the static
  `API_TOKEN` list only, and deliberately refuses anything carrying the scoped
  token prefix. There is no database-backed fallback.
* **Scopes are never checked.** The interceptor asks "is this a valid token",
  never "is this token allowed to read monitors". Since a static `API_TOKEN` is
  already unscoped everywhere else, this is not a privilege escalation against
  REST — but it does mean there is no read-only gRPC credential to hand out.
  Every gRPC client needs a full-access static token.

## Service reference

### `GetMonitor`

One monitor by ID.

| | |
| --- | --- |
| Request | `GetMonitorRequest { int64 id }` |
| Response | `Monitor` |
| Errors | `NotFound` if no monitor has that id; `Internal` on a store failure |

```sh
grpcurl -plaintext -import-path proto -proto sorobeacon/v1/sorobeacon.proto \
  -d '{"id": 1}' 127.0.0.1:9090 sorobeacon.v1.MonitorService/GetMonitor
```

`Monitor` carries `id`, `name`, `contract_ids`, `enabled`, `created_at` and
`channel_ids`. Channel **configuration is never returned** — `channel_ids` is
the attachment list, not the credentials behind it — consistent with every
other surface. See [Channels](api.md#channels).

### `ListMonitors`

A page of monitors, newest first.

| | |
| --- | --- |
| Request | `ListMonitorsRequest { bool enabled_only, int32 limit, int64 cursor }` |
| Response | `ListMonitorsResponse { repeated Monitor monitors }` |
| Paging | keyset on `id`, descending |

`limit` outside 1–500 is silently replaced with 50; it is not an error, so a
client cannot tell a clamped page from a requested one. There is no `sort`
argument, so the order is always id-descending — the REST equivalent's `sort`
is not reachable from here.

**There is no `next_cursor` in the response.** Page by taking the `id` of the
last monitor in the array and passing it as `cursor` in the next request; the
store returns rows with `id < cursor`. A short page (fewer than `limit`
returned) means the end. This is the same keyset the REST API uses, where the
cursor is returned to you as `next_cursor` instead of left for you to derive.

### `ListAlerts`

A page of alerts, newest first.

| | |
| --- | --- |
| Request | `ListAlertsRequest { int64 monitor_id, int64 rule_id, int32 limit, int64 cursor }` |
| Response | `ListAlertsResponse { repeated Alert alerts }` |
| Paging | keyset on `(created_at, id)`, descending |

`limit` outside 1–500 becomes 50. `monitor_id` and `rule_id` are optional
filters; zero means no filter on that field. The cursor works as on
`ListMonitors`, and the response carries no `next_cursor`: use the last alert's
`id`.

The REST equivalent also filters on `contract_id`, free-text `q`, `from`, `to`
and `severity`, and can sort ascending. None of those are reachable from gRPC;
[Alerts](api.md#alerts) documents them for clients that need them.

`Alert` carries `id`, `monitor_id`, `rule_id`, `event_id`, `payload` and
`created_at`. It does **not** carry the fields the REST API returns alongside
those: `severity`, `ledger`, `retracted_at`, `backfilled`, `inhibited_by_rule_id`,
`acknowledged_at`, `network`, enrichment, or the suppression fields. A client
that needs to tell a retracted alert from a live one has to use REST.

### `StreamAlerts`

Server-streaming subscription to new alerts. The one method here intended for
long-lived connections, and the only one whose interceptor currently runs — see
the warning at the top.

| | |
| --- | --- |
| Request | `StreamAlertsRequest { int64 monitor_id }` |
| Response | a stream of `Alert` messages |
| Filter | `monitor_id` only; there is no `rule_id` filter |

#### Streaming semantics

The implementation is a **poll, not a subscription bus**, and the difference is
worth being precise about before you depend on it. Answering each question
against `internal/grpc/server.go`:

**Does a client receive alerts raised before it subscribed? No.** On subscribe
the server reads the single most recent matching alert and remembers its id as
the starting point. That alert is *not* sent — it only anchors the cursor — so
the stream begins at "from now". History is not replayed.

**What happens while a client is slow?** Alerts are not dropped. Each tick
fetches up to 100 rows newer than the last id actually *sent*, so if more than
100 alerts arrive between ticks the remainder arrive on the next one, in
order. Delivery is bounded by the database poll, not by a per-client buffer
that can overflow.

**What happens on disconnect, and will a client miss alerts?** It will. The
stream terminates when the client goes away, and there is no resume cursor and
no replay: reconnecting re-anchors to "now", so any alert raised during the
gap is gone from the stream permanently. Alerts are still stored — read them
back with `ListAlerts` over the gap window if that matters, or use the REST
API's SSE endpoint, which is fed by an in-process broadcast instead of a poll
and so has neither the two-second latency nor the gap.

**Other behaviour worth knowing:**

* The poll interval is 2 seconds, so an alert reaches a subscriber up to ~2s
  after it is stored. It is not sub-second like the SSE path.
* A failed poll (a database blip) is logged and retried on the next tick. The
  stream stays open; you do not see the error.
* The stream has no server-side keepalive and no idle timeout. A client behind
  a proxy that drops idle connections should send its own keepalive pings.
* `monitor_id: 0` — or omitting it — means every monitor.
* **Workspaces are not honoured.** The interceptor calls `Authenticator.Verify`,
  which returns a yes/no and discards the workspace the credential is bound
  to, so no workspace ever reaches the request context. Store queries with no
  workspace in context resolve to the **default** workspace. On a single
  workspace instance, as on the default deployment, this changes nothing. On a
  multi-workspace one, a credential bound to another workspace via
  `WORKSPACE_TOKENS` will authenticate and then read the default workspace
  rather than its own.

## Error codes

The server maps errors onto gRPC status codes and does not leak internals: a
store failure is `Internal` with the message `internal error`, never the
underlying error text.

| Code | When | Retry? |
| --- | --- | --- |
| `Unauthenticated` | Missing metadata, missing `authorization`, a header that is not `Bearer …`, or a token the authenticator rejects. | No. Fix the credential. |
| `NotFound` | `GetMonitor` with an unknown id. | No. |
| `InvalidArgument` | Not produced by the server; a malformed field in a request you send. | No. |
| `Internal` | A store failure — and, today, every unary request, because the request cannot be decoded. | No, not for the decode failure. |
| `Unimplemented` | An unknown method on the service. | No. |
| `Unavailable` | Transport-level: the server is down or the connection dropped. | Yes, with backoff. |
| `DeadlineExceeded` | You set a deadline that expired. | Yes, if the work is idempotent — a read is. |

## Protobuf definitions and client generation

The schema is in this repository, so a client can be generated from it without
any dependency on the server:

```
proto/sorobeacon/v1/sorobeacon.proto
```

```sh
protoc --go_out=. --go-grpc_out=. proto/sorobeacon/v1/sorobeacon.proto
```

**Server reflection is not enabled.** `grpcurl` cannot discover the service on
its own and says so:

```
$ grpcurl -plaintext 127.0.0.1:9090 list
Failed to list services: server does not support the reflection API
```

Pass `-import-path proto -proto sorobeacon/v1/sorobeacon.proto` on every call,
as the examples above do, or generate a client and skip grpcurl entirely.
Enabling reflection would remove that flag from everyone's command line; it is
left off because reflection is an unauthenticated schema dump.

Two things the repository does not currently do, both reported rather than
worked around here: the generated Go stubs are not committed, and the `make proto`
target that `sorobeacon.proto` and the package comment both refer to does not
exist.

## Relationship to the REST API

The two surfaces are not equivalent, and the gRPC one is deliberately the
smaller. Choose by what you need:

| | REST | gRPC |
| --- | --- | --- |
| Write monitors, rules, channels | yes | **no, by design** |
| Read monitors, read alerts | yes | yes |
| Live alert stream | SSE at `/alerts/stream`, fed by an in-process broadcast | `StreamAlerts`, a 2-second database poll |
| Cursor | `next_cursor` returned | you derive it from the last id |
| Sort and filter | full | monitors: `enabled_only` only; alerts: `monitor_id`, `rule_id` |
| Credential | static or scoped token, scopes enforced | static token only, no scopes, no workspace scoping |
| TLS | as the deployment terminates it | none — cleartext only |

**Prefer REST unless you have a reason not to.** It is the complete surface, it
is what the dashboard uses, it is the only one that enforces scopes, and every
field the store holds is reachable through it. gRPC exists for the one thing
REST does not do well from a non-browser client — a long-lived alert
subscription with a typed message — and today even that method is behind the
warning at the top of this page.

[grpcurl]: https://github.com/fullstorydev/grpcurl