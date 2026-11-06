# `@sorotrail/sorobeacon-client`

Generated TypeScript client for the [SoroBeacon](https://github.com/sorotrail/SoroBeacon) HTTP API (`/api/v1`), derived directly from [`internal/api/openapi.json`](../../internal/api/openapi.json).

## Highlights

- **100% OpenAPI coverage**: Typed methods and request/response interfaces for every route documented in `internal/api/openapi.json` (monitors, rules, dry-runs, escalations, channels, channel signing keys, maintenance windows, inhibitions, alerts, SSE streaming, CSV/NDJSON exports, deliveries, dead letters, federated ingest, triggers, tokens, templates, audit logs, health/poller/stats/version, and persisted GraphQL operations).
- **Lossless wide integers (`Int64`, `Int64Value`)**: `parseJSONPreservingWideInts` quotes JSON integer tokens outside `[Number.MIN_SAFE_INTEGER, Number.MAX_SAFE_INTEGER]` ($2^{53}-1$) before `JSON.parse` can round their low bits, and `stringifyJSONWithWideInts` serializes `bigint` values cleanly on the wire. Use `toBigInt(val)` to convert any `Int64` value to a native `bigint`.
- **Structured JSON event payloads (`AlertPayload`, `JsonObject`)**: Decoded contract event payloads and rule parameters are typed as structured JSON objects rather than opaque strings.
- **Redacted secrets (`ChannelInput` vs `Channel`, `CreatedAPIToken` vs `APIToken`)**: Write-only secret fields (`ChannelInput.config`) are required on creation and accepted on update (`ChannelUpdateInput`), while read models (`Channel`, `APIToken`) type them as `never` (`config?: never`, `token?: never`) so callers cannot accidentally rely on secrets that the server never echoes back.
- **Typed error taxonomy (`SoroBeaconAPIError`)**: Non-2xx responses throw `SoroBeaconAPIError` with `.status`, `.code` (`ErrorCode`), `.requestId`, `.details` (`ErrorDetail[]`), and predicate helpers (`isNotFound()`, `isAlreadyExists()`, `isInvalidArgument()`, `isConflict()`, `isUnauthenticated()`, `isPermissionDenied()`, `isRateLimited()`, `isQuotaExceeded()`, `isPayloadTooLarge()`, `isServiceUnavailable()`).
- **Async-iterable cursor pagination**: `paginateMonitors`, `paginateTrashMonitors`, `paginateChannels`, `paginateAlerts`, and `paginateAuditLogs` walk `next_cursor` pages transparently via `for await (const item of client.paginateAlerts(...))`.

## Regenerating from `internal/api/openapi.json`

Whenever `internal/api/openapi.json` changes, regenerate `src/index.ts` with:

```bash
go generate ./clients/typescript/...
# or:
go test ./clients/typescript -run TestTypeScriptClientMatchesOpenAPISpec -update
```

`go test ./...` runs `TestTypeScriptClientMatchesOpenAPISpec` in CI and fails if `clients/typescript/src/index.ts` drifts from `internal/api/openapi.json`.

## Usage Example

See [`examples/usage.ts`](./examples/usage.ts) for a complete, runnable example:

```ts
import { SoroBeaconClient, SoroBeaconAPIError, toBigInt } from "./src/index.ts";

const client = new SoroBeaconClient({
  baseUrl: "http://localhost:8080",
  token: process.env.SOROBEACON_TOKEN,
});

const monitor = await client.createMonitor({
  name: "usdc-watch",
  contract_ids: ["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"],
  enabled: true,
});

for await (const alert of client.paginateAlerts({ monitor_id: monitor.id })) {
  console.log(toBigInt(alert.id), alert.event_name, alert.payload);
}
```
