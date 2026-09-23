# Maintenance windows

Every planned upgrade, migration or load test produces a burst of alerts that
everyone knows about in advance. Without a silence mechanism, operators
disable monitors and forget to re-enable them — which is how real incidents
get missed.

A **maintenance window** suppresses **delivery**, not **detection**: an alert
raised inside the window is still stored and still visible on the alerts page,
marked **suppressed** with the window's reason. Detection keeps running, so
the record survives, while people are spared the noise.

## Scope

A window applies to one of three scopes:

| Scope | Suppresses |
| --- | --- |
| `global` | every alert |
| `monitor` | alerts for one monitor (all of its contracts) |
| `contract` | alerts for one contract ID, across every monitor |

When more than one window covers an alert, the most specific wins for the
reason shown: contract, then monitor, then global.

## Bounds are required

Every window has an explicit `end_at`, and `end_at` must be after `start_at`.
An open-ended silence is rejected at create time — forgotten silences are the
exact failure mode this feature exists to prevent.

The interval is half-open: `start_at` is included, `end_at` is not.

## Managing windows

### Dashboard

The **Maintenance** page lists active and upcoming windows and lets you create
or delete them. Times are entered and shown in UTC.

### API

```sh
# Create a global window
curl -s -X POST localhost:8080/api/v1/maintenance-windows -d '{
  "reason": "planned upgrade",
  "scope": "global",
  "start_at": "2026-09-23T22:00:00Z",
  "end_at": "2026-09-24T02:00:00Z"
}'

# A monitor-scoped window
curl -s -X POST localhost:8080/api/v1/maintenance-windows -d '{
  "reason": "load test",
  "scope": "monitor",
  "monitor_id": 1,
  "start_at": "2026-09-23T22:00:00Z",
  "end_at": "2026-09-24T02:00:00Z"
}'

# A contract-scoped window
curl -s -X POST localhost:8080/api/v1/maintenance-windows -d '{
  "reason": "contract migration",
  "scope": "contract",
  "contract_id": "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
  "start_at": "2026-09-23T22:00:00Z",
  "end_at": "2026-09-24T02:00:00Z"
}'

curl -s 'localhost:8080/api/v1/maintenance-windows?active=true'
curl -s 'localhost:8080/api/v1/maintenance-windows?upcoming=true'
curl -s -X DELETE localhost:8080/api/v1/maintenance-windows/1
```

The suppression check runs on the delivery path as a single indexed lookup, so
a window adds one query per alert — not one per channel.
