# Configuration

All configuration comes from environment variables. `.env.example` in the repo is a ready-to-copy template. The complete operator table (types, required vs optional, secrets, `SOURCE_MODE`-only variables) is [Environment variable reference](../configuration.md).

| Variable | Default | Description |
| --- | --- | --- |
| `SOURCE_MODE` | `rpc` | `rpc` (standalone: poll the RPC) or `sorotrail` (upstream: read a SoroTrail indexer). |
| `SOROTRAIL_URL` | — | SoroTrail indexer base URL. Required when `SOURCE_MODE=sorotrail`. |
| `NETWORK` | `testnet` | `testnet` \| `mainnet` \| `futurenet` \| `custom`. Selects the network preset (RPC endpoint + passphrase). |
| `RPC_URL` | per `NETWORK` | Stellar RPC endpoint (JSON-RPC 2.0). Overrides the preset. |
| `NETWORK_PASSPHRASE` | per `NETWORK` | Overrides the preset passphrase. Required with `NETWORK=custom`. |
| `DATABASE_URL` | _(required)_ | Postgres URL (`postgres` or `postgresql` scheme), e.g. `postgres://user:pass@host:5432/sorobeacon?sslmode=disable`. Validated at load; errors name the variable and never echo the password. |
| `DATABASE_MAX_CONNS` | pgx default | Maximum connections in the pool. `0` or unset leaves the driver default. |
| `DATABASE_MIN_CONNS` | pgx default | Minimum connections in the pool. `0` or unset leaves the driver default. Rejected when greater than `DATABASE_MAX_CONNS` if both are set. |
| `DATABASE_MAX_CONN_LIFETIME` | pgx default | How long a connection may be reused. Go duration (`1h`, `30m`). `0` or unset leaves the driver default. |
| `DATABASE_MAX_CONN_IDLE_TIME` | pgx default | How long an idle connection is kept. Go duration. `0` or unset leaves the driver default. |
| `POLL_INTERVAL` | `5s` | How often the poller calls `getEvents`. Minimum `1s`. |
| `HTTP_ADDR` | `:8080` | Listen address (`host:port`) for the API and dashboard. Empty host means all interfaces. Validated at load. |
| `MONITOR_SILENT_AFTER` | `24h` | How long since `last_matched_at` (event ledger close time) before the monitors list marks a monitor silent. |
| `HTTP_ADDR` | `:8080` | Listen address for the API and dashboard. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` (structured JSON via `log/slog`). |
| `READYZ_LAG_THRESHOLD` | `0` (disabled) | Fail `/readyz` when poller ledger lag (chain tip minus last processed ledger) exceeds this. Unset or `0` leaves existing probes unchanged. |
| `ALERT_RETENTION` | unset (keep forever) | Age after which alerts are deleted in batches of 1000 (`90d`, `24h`, …). `delivery_attempts` follow via `ON DELETE CASCADE`. Unset preserves current behaviour: nothing is pruned. |

## Where secrets live

Channel secrets — webhook URLs, bot tokens, SMTP credentials — are stored in each channel's `config` JSON in Postgres, **not** in environment variables. SoroBeacon never logs them and never returns them from the API or renders them in the dashboard.

{% hint style="warning" %}
The MVP has **no API authentication** and stores channel secrets **unencrypted** in the database. Run SoroBeacon on a trusted network (or behind an authenticating reverse proxy) and restrict database access. Both hardening items are open contributor issues with designed-in extension points.
{% endhint %}

## Behavior under errors

* **RPC failures** back off exponentially, capped at 10× the poll interval, then recover automatically.
* **Delivery failures** are retried 3 times per channel with exponential backoff (1s, 2s, 4s); every attempt is recorded in `delivery_attempts`.
* **Restarts** resume from the last checkpointed ledger; the `(rule_id, event_id)` dedup guard guarantees a replayed window never re-alerts.
