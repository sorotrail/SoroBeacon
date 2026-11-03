# `sorobeacon` Python Client

Generated Python client for the [SoroBeacon](https://github.com/sorotrail/SoroBeacon) HTTP API (`/api/v1`), derived directly from [`internal/api/openapi.json`](../../internal/api/openapi.json).

## Packaging & Installation

Following the same repository-first packaging decision as [`clients/typescript`](../typescript/README.md), `sorobeacon` has zero third-party runtime dependencies (stdlib only) and is consumed directly from the repository:

```bash
pip install "./clients/python"
# or from Git:
pip install "git+https://github.com/sorotrail/SoroBeacon.git#subdirectory=clients/python"
```

## Highlights

- **100% OpenAPI coverage**: Typed dataclasses and methods for every route in `internal/api/openapi.json` (monitors, rules, dry-runs, escalations, channels, channel signing keys, maintenance windows, inhibitions, alerts, SSE streaming, CSV/NDJSON exports, deliveries, dead letters, federated ingest, triggers, tokens, templates, audit logs, health/poller/stats/version, and persisted GraphQL operations).
- **Lossless wide integers (`WideInt`, `Int64`, `Decimal`)**: Wide Soroban integers (`i128`, `u128`, `i256`, `u256`) and 64-bit resource/ledger IDs are typed as `str | Decimal` (`WideInt`) or `int | str | Decimal` (`Int64`), never `float`. `parse_json_preserving_wide_ints` parses all JSON decimals via `decimal.Decimal`, and `parse_wide_int` / `parse_wide_decimal` explicitly reject `float` and `bool` with `TypeError`.
- **Structured JSON event payloads (`Alert.payload: JSONObject`, `AlertPayload`)**: Decoded contract event payloads and rule parameters are typed as structured `JSONObject` (`dict[str, JSONValue]`) rather than opaque strings.
- **Redacted secrets (`ChannelInput` vs `Channel`, `CreatedAPIToken` vs `APIToken`)**: Write-only secret fields (`ChannelInput.config`, `ChannelUpdateInput.config`) are accepted on write, while read models (`Channel`, `APIToken`) never include `config` or `token` so the types match what the server returns.
- **Typed error taxonomy (`SoroBeaconAPIError`)**: Non-2xx responses raise `SoroBeaconAPIError` subclasses (`NotFoundError`, `BadRequestError`, `ConflictError`, `UnauthenticatedError`, `PermissionDeniedError`, `AlreadyExistsError`, `PayloadTooLargeError`, `RateLimitedError`, `QuotaExceededError`, `ServiceUnavailableError`, `InternalServerError`) with `.status_code`, `.code` (`ErrorCode`), `.request_id`, `.details` (`list[ErrorDetail]`), and predicate helpers (`.is_not_found()`, `.is_invalid_argument()`, etc.).
- **Transparent cursor pagination (`CursorIterator`)**: `iter_monitors`, `iter_trash_monitors`, `iter_channels`, `iter_alerts`, and `iter_audit_logs` return a standard Python `Iterator` that pages transparently via `next_cursor`.

## Regenerating from `internal/api/openapi.json`

Whenever `internal/api/openapi.json` changes, regenerate `sorobeacon/_generated.py` with:

```bash
go generate ./clients/python/...
# or:
go test ./clients/python -run TestPythonClientMatchesOpenAPISpec -update
```

`go test ./...` runs `TestPythonClientMatchesOpenAPISpec` in CI and fails if `clients/python/sorobeacon/_generated.py` drifts from `internal/api/openapi.json`.

## Usage Example

See [`examples/usage.py`](./examples/usage.py) for a complete, runnable example:

```python
import os
from sorobeacon import MonitorInput, SoroBeaconClient, parse_wide_decimal

client = SoroBeaconClient(
    base_url=os.environ.get("SOROBEACON_URL", "http://localhost:8080"),
    token=os.environ.get("SOROBEACON_TOKEN"),
)

monitor = client.create_monitor(
    MonitorInput(
        name="usdc-watch",
        contract_ids=["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"],
        enabled=True,
    )
)

for alert in client.iter_alerts(monitor_id=monitor.id):
    data = alert.payload.get("data")
    if isinstance(data, dict) and isinstance(data.get("amount"), str):
        print(alert.id, alert.event_name, parse_wide_decimal(data["amount"], decimals=7))
```
