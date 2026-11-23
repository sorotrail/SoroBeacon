# Federation

Forwards alerts to **another SoroBeacon instance**. It is the only channel
that talks to SoroBeacon itself rather than to a chat service, a mailbox or
an endpoint of your own: the sending instance decides what matched, and the
receiving instance is handed the result.

## Status

Federation delivers when the sending instance has `INSTANCE_NAME` set. The
sender stamps that value as the `origin` on every alert; the receiving
instance requires a non-empty origin and answers `201` when it accepts the
payload.

If `INSTANCE_NAME` is unset, creating a federation channel fails closed with
an actionable error. The rest of the instance keeps running; only the
federation channel is unavailable. This is deliberate: sending a payload with
an empty origin would be rejected by the receiver on every attempt.

A related receiver bug used to store the alert against the sender's monitor
and rule ids, which do not exist in its own database, so the insert failed and
the receiver answered `404`
([#526](https://github.com/sorotrail/SoroBeacon/issues/526)). The receiver now
stores a federated alert as what it is — an alert from another instance,
belonging to no local monitor and no local rule — and answers `201`.

## What works

* The channel is created and its configuration is checked: a missing `url`,
  `token` or `INSTANCE_NAME` is a `400` at create time, not a failure at
  delivery time.
* The sender stamps `origin` with its `INSTANCE_NAME`, so the receiving
  instance can attribute and loop-check the alert.
* The sender builds the right request: `POST <url>/api/v1/ingest` with
  `Authorization: Bearer <token>` and the alert wrapped in the federated
  envelope.
* The receiving instance authenticates the caller, enforces the hop limit,
  stores the sender's origin with the alert, and refuses a payload that is not
  a federated alert.
* The failure is visible: the delivery attempt is recorded, and the channel's
  health (`consecutive_failures`, `last_error`) says what the other instance
  answered.

## Setup

1. On the **sending** instance, set `INSTANCE_NAME` to a stable name that is
   unique across the instances that federate with each other, for example
   `beacon-a`, and restart the process. The value is stamped as the alert's
   `origin`; changing it changes how the receiver attributes later alerts.
2. On the **receiving** instance, take a token that may write alerts. An
   instance-wide `API_TOKEN` works; a scoped token needs the `alerts:write`
   scope (see [Authorization](../reference/authorization.md)). Give the sender
   its own token, not the operator's.
3. On the **sending** instance, create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "to-central",
  "type": "federation",
  "config": {"url": "http://beacon-b.internal:8080", "token": "s3cr3t-token-value"}
}'
```

4. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/4/test
```

A working federation answers `{"status":"sent"}`. If it answers a `502`,
read the receiving instance's error inside it; the troubleshooting table
below maps the common ones.

## Config

| Key | Required | Description |
| --- | --- | --- |
| `url` | yes | Base URL of the **receiving** instance, e.g. `http://beacon-b.internal:8080`. The sender appends `/api/v1/ingest`; a trailing slash is ignored. Treated as a secret (it often embeds a token in a path). |
| `token` | yes | Bearer token the sender presents to the receiving instance. Treated as a secret. |

Plain `http://` is accepted, so two instances on a private network work
without TLS. Do not send the token over plain `http://` on an untrusted
network — put the receiving instance behind TLS, or reach it over a private
link.

## The wire format

Every delivery is a `POST` to `<url>/api/v1/ingest` with the alert wrapped
in an envelope that carries the origin and the hop count:

```json
{
  "alert": {
    "id": 42,
    "monitor_id": 1,
    "monitor_name": "Token treasury",
    "rule_id": 3,
    "rule_type": "value_threshold",
    "event_id": "0000015985348674617345-0000000001",
    "contract_id": "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
    "event_name": "transfer",
    "ledger": 3721765,
    "tx_hash": "4f2a...",
    "payload": {"topics": ["transfer", "..."], "value": "..."},
    "created_at": "2026-07-21T09:41:32Z"
  },
  "origin": "beacon-a",
  "hop_count": 1
}
```

The headers are `Authorization: Bearer <token>` and `Content-Type:
application/json`.

## The receiving side

`POST /api/v1/ingest` is a normal API route: it needs a credential, and a
scoped token needs the `alerts:write` scope. It answers `201` with the
outcome, the stored alert's id and the origin it was given, or `400` when
the payload is not a federated alert it will accept:

| Condition | Answer |
| --- | --- |
| No `event_id` in the alert | `400 {"error":"event_id is required"}` |
| No `origin` | `400 {"error":"origin is required"}` |
| `hop_count` at or above the limit | `400 {"error":"hop limit reached; dropping alert to prevent loop"}` |
| A severity outside the vocabulary | `400 {"error":"invalid severity"}` |
| The payload cannot be stored here | `422` — a client error, and the sender must not retry it |

**The monitor and rule ids are the sender's, and they stay the sender's.**
They are kept as the stored alert's `source_monitor_id` and `source_rule_id`,
and the alert itself belongs to no local monitor and no local rule: its own
`monitor_id` and `rule_id` are zero, and `federated` is true. So a payload
that happens to name ids this instance also has does not attach itself to
them — the alert is never mistaken for a match of a local rule, and it never
shows up in a local monitor's statistics or exports.

**The alert lands in the workspace of the credential that delivered it**,
never in one the payload names: the payload is the other instance's data and
cannot choose this instance's tenancy.

**Rules are not re-evaluated on the receiver.** The sending instance already
decided what matched; the receiver stores the result. An event that would
match one of the receiver's own rules does not fire that rule, and the
receiver's rules are not consulted at all.

**A federated alert is stored, not delivered.** The ingest endpoint writes
the alert row and stops. It does not fan the alert out to the receiving
instance's channels, so a federated alert shows up in the receiver's alert
history (`GET /api/v1/alerts`) but does not page anybody there. Delivery is
the polling path's job, and the ingest path does not go through it.

The payload of a stored federated alert carries five extra keys, so the
receiver's dashboard and API can tell where it came from:

```json
{
  "contract_id": "CA7QYNF7…",
  "_federated": true,
  "_federation_origin": "beacon-a",
  "_federation_hops": 1,
  "_federation_monitor_id": 1,
  "_federation_rule_id": 3
}
```

## The hop limit

`MaxHopCount` is 8 (`internal/notify/federation.go`). It exists so a chain
of instances forwarding to each other terminates instead of amplifying an
alert forever: the receiver refuses any alert whose `hop_count` has reached
the limit.

The counter does not move today. The sender always sends `hop_count: 1`, and
nothing re-forwards a federated alert: a received alert is stored and stops
there. The limit is enforced on the receiving side and is ready for the
forwarding that has not been built.

## Troubleshooting

| Symptom | What it means |
| --- | --- |
| `400 {"details":[{"field":"config","reason":"federation: token is required"}]}` at create time | The channel config is incomplete. `url` and `token` are both required. |
| `400` with `federation: INSTANCE_NAME is required` at create time | Set `INSTANCE_NAME` on the sending instance and restart before creating the federation channel. |
| `502` with `federation: INSTANCE_NAME is required` on an existing channel | The channel predates `INSTANCE_NAME`, or the process restarted without it. Set it and restart; the existing channel uses the new value on its next delivery. |
| `502` with `federation: status 422: …` | The other instance could not store the alert: the payload violates one of its constraints. A 422 is permanent, so the delivery is dead-lettered rather than retried. |
| `502` with a connection error | The other instance is not reachable at the `url` you configured, or it is behind TLS and the URL says `http://`. The path is `<url>/api/v1/ingest` — configure the base URL, not the full path. |
| The channel's `consecutive_failures` climbs | Every attempt failed; `last_error` carries the other instance's answer, which is the fastest way to tell a transport problem from a payload problem. |

The delivery attempt itself is on the sender:
`GET /api/v1/alerts/{id}/deliveries` lists the attempts for the alert that
triggered the delivery, and `GET /api/v1/channels/{id}` carries the channel's
health.
