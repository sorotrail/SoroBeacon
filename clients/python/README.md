# `sorobeacon-client`

Generated Python client for the [SoroBeacon](https://github.com/sorotrail/SoroBeacon) HTTP API (`/api/v1`), derived directly from [`internal/api/openapi.json`](../../internal/api/openapi.json).

## Highlights

- **100% OpenAPI coverage**: Typed methods and `TypedDict` request/response contracts for every route documented in `internal/api/openapi.json` (monitors, rules, dry-runs, escalations, channels, channel signing keys, maintenance windows, inhibitions, alerts, SSE streaming, CSV/NDJSON exports, deliveries, dead letters, federated ingest, triggers, tokens, templates, audit logs, health/poller/stats/version, and persisted GraphQL operations).
- **Lossless wide integers (`Int64`, `Int64Value`, `WideInt`)**: Wide integers are typed as `Decimal | str | int` (never `float`). `parse_json_preserving_wide_ints` preserves JSON integer tokens outside `[-9007199254740991, 9007199254740991]` ($2^{53}-1$) as exact base-10 strings and parses fractional numbers as `Decimal` rather than `float`. `stringify_json_with_wide_ints` emits `Decimal` and arbitrary-precision `int` values as exact JSON integer tokens while rejecting `float` inputs. Use `to_decimal(val)` or `to_int(val)` for lossless arithmetic.
- **Structured JSON event payloads (`AlertPayload`, `JsonObject`)**: Decoded contract event payloads and rule parameters are typed as structured JSON mappings rather than opaque strings.
- **Redacted secrets (`ChannelInput` vs `Channel`, `CreatedAPIToken` vs `APIToken`)**: Write-only secret fields (`ChannelInput["config"]`) are required on creation and accepted on update (`ChannelUpdateInput`), while read models (`Channel`, `APIToken`) omit them so type checkers reject attempts to read `channel["config"]` or `token["token"]` on read responses.
- **Typed error taxonomy (`SoroBeaconAPIError`)**: Non-2xx responses raise `SoroBeaconAPIError` with `.status`, `.code` (`ErrorCode`), `.request_id`, `.details` (`list[ErrorDetail]`), and predicate helpers (`is_not_found()`, `is_already_exists()`, `is_invalid_argument()`, `is_conflict()`, `is_unauthenticated()`, `is_permission_denied()`, `is_rate_limited()`, `is_quota_exceeded()`, `is_payload_too_large()`, `is_service_unavailable()`).
- **Iterator cursor pagination**: `paginate_monitors`, `paginate_trash_monitors`, `paginate_channels`, `paginate_alerts`, and `paginate_audit_logs` walk `next_cursor` pages transparently as standard Python iterators (`for alert in client.paginate_alerts(...):`).

## Packaging Decision

Matching `@sorotrail/sorobeacon-client` in `clients/typescript`, `sorobeacon-client` has zero third-party runtime dependencies (relying only on the Python standard library) and is consumed directly from the repository:

```bash
pip install ./clients/python
# or from git:
pip install "git+https://github.com/sorotrail/SoroBeacon.git#subdirectory=clients/python"
```

## Regenerating from `internal/api/openapi.json`

Whenever `internal/api/openapi.json` changes, regenerate `sorobeacon/__init__.py` with:

```bash
go generate ./clients/python/...
# or:
go test ./clients/python -run TestPythonClientMatchesOpenAPISpec -update
```

`go test ./...` runs `TestPythonClientMatchesOpenAPISpec` in CI and fails if `clients/python/sorobeacon/__init__.py` drifts from `internal/api/openapi.json`.

## Usage Example

See [`examples/usage.py`](./examples/usage.py) for a complete, runnable example:

```python
import os
from sorobeacon import SoroBeaconClient, SoroBeaconAPIError, to_decimal, to_int

client = SoroBeaconClient(
    base_url="http://localhost:8080",
    token=os.environ.get("SOROBEACON_TOKEN"),
)

monitor = client.create_monitor({
    "name": "usdc-watch",
    "contract_ids": ["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"],
    "enabled": True,
})

for alert in client.paginate_alerts({"monitor_id": monitor["id"]}):
    print(to_int(alert["id"]), to_decimal(alert["id"]), alert["event_name"], alert["payload"])
```
