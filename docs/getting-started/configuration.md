# Configuration

All configuration comes from environment variables. `.env.example` in the repo is a ready-to-copy template.

| Variable | Default | Description |
| --- | --- | --- |
| `SOURCE_MODE` | `rpc` | `rpc` (standalone: poll the RPC) or `sorotrail` (upstream: read a SoroTrail indexer). |
| `SOROTRAIL_URL` | — | SoroTrail indexer base URL. Required when `SOURCE_MODE=sorotrail`. |
| `NETWORK` | `testnet` | `testnet` \| `mainnet` \| `futurenet` \| `custom`. Selects the network preset (RPC endpoint + passphrase). |
| `RPC_URL` | per `NETWORK` | Stellar RPC endpoint (JSON-RPC 2.0). Overrides the preset. |
| `NETWORK_PASSPHRASE` | per `NETWORK` | Overrides the preset passphrase. Required with `NETWORK=custom`. |
| `DATABASE_URL` | _(required)_ | Postgres connection string, e.g. `postgres://user:pass@host:5432/sorobeacon?sslmode=disable` |
| `POLL_INTERVAL` | `5s` | How often the poller calls `getEvents`. Minimum `1s`. |
| `HTTP_ADDR` | `:8080` | Listen address for the API and dashboard. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` (structured JSON via `log/slog`). |

## Where secrets live

Channel secrets — webhook URLs, bot tokens, SMTP credentials — are stored in each channel's `config` JSON in Postgres, **not** in environment variables. SoroBeacon never logs them and never returns them from the API or renders them in the dashboard.

{% hint style="warning" %}
The MVP has **no API authentication** and stores channel secrets **unencrypted** in the database. Run SoroBeacon on a trusted network (or behind an authenticating reverse proxy) and restrict database access. Both hardening items are open contributor issues with designed-in extension points.
{% endhint %}

## Behavior under errors

* **RPC failures** back off exponentially, capped at 10× the poll interval, then recover automatically.
* **Delivery failures** are retried 3 times per channel with exponential backoff (1s, 2s, 4s); every attempt is recorded in `delivery_attempts`.
* **Restarts** resume from the last checkpointed ledger; the `(rule_id, event_id)` dedup guard guarantees a replayed window never re-alerts.
