# Configuration

All configuration comes from environment variables. `.env.example` in the repo is a ready-to-copy template. The complete operator table (types, required vs optional, secrets, `SOURCE_MODE`-only variables) is [Environment variable reference](../configuration.md).

| Variable | Default | Description |
| --- | --- | --- |
| `SOURCE_MODE` | `rpc` | `rpc` (standalone: poll the RPC) or `sorotrail` (upstream: read a SoroTrail indexer). |
| `SOROTRAIL_URL` | — | SoroTrail indexer base URL. Required when `SOURCE_MODE=sorotrail`. |
| `NETWORK` | `testnet` | `testnet` \| `mainnet` \| `futurenet` \| `custom`. Selects the network preset (RPC endpoint + passphrase). |
| `RPC_URL` | per `NETWORK` | Stellar RPC endpoint (JSON-RPC 2.0). Overrides the preset. |
| `RPC_URLS` | — (uses `RPC_URL`) | Ordered, comma-separated RPC endpoints to fail over between. Takes priority over `RPC_URL` when set. Every endpoint must be on the configured network or startup fails. |
| `NETWORK_PASSPHRASE` | per `NETWORK` | Overrides the preset passphrase. Required with `NETWORK=custom`. |
| `DATABASE_URL` | _(required)_ | Backend URL selected by scheme. `postgres` / `postgresql` → a Postgres server, e.g. `postgres://user:pass@host:5432/sorobeacon?sslmode=disable`. `sqlite` → a single database file, e.g. `sqlite:///var/lib/sorobeacon/sorobeacon.db`, for a node with no Postgres. Validated at load; errors name the variable and never echo the password. |
| `DATABASE_MAX_CONNS` | pgx default | Maximum connections in the **Postgres** pool. `0` or unset leaves the driver default. Setting it with a `sqlite` URL fails at startup. |
| `DATABASE_MIN_CONNS` | pgx default | Minimum connections in the pool. `0` or unset leaves the driver default. Rejected when greater than `DATABASE_MAX_CONNS` if both are set. |
| `DATABASE_MAX_CONN_LIFETIME` | pgx default | How long a connection may be reused. Go duration (`1h`, `30m`). `0` or unset leaves the driver default. |
| `DATABASE_MAX_CONN_IDLE_TIME` | pgx default | How long an idle connection is kept. Go duration. `0` or unset leaves the driver default. |
| `REPLICA_DATABASE_URL` | unset (reads go to the primary) | Postgres connection string of a read replica serving the read-only dashboard queries (monitor list, alert search, stats, alert counts by day). Must be a `postgres`/`postgresql` URL other than `DATABASE_URL`; both are validated at startup, and a `sqlite` `DATABASE_URL` with this set is rejected. A replica that cannot be reached at boot fails startup; one that goes away later degrades to the primary. |
| `API_TOKEN` | unset (authentication off) | Comma-separated static bearer token(s) for `/api/v1`, and the credential the dashboard's sign-in page accepts. Unset leaves both open and logs one warning at startup. `GET /health`, `/livez` and `/readyz` are exempt. See [API authentication](../configuration.md#api-authentication). |
| `CONFIG_ENCRYPTION_KEY` | unset (encryption off) | Base64 AES-GCM key that encrypts each channel's `config` at rest. Must decode to 16, 24 or 32 bytes (32 recommended); validated at startup. Generate with `openssl rand -base64 32`. Unset keeps plaintext and logs one startup warning. |
| `POLL_INTERVAL` | `5s` | How often the poller calls `getEvents`. Minimum `1s`. |
| `HTTP_ADDR` | `:8080` | Listen address (`host:port`) for the API and dashboard. Empty host means all interfaces. Validated at load. |
| `MONITOR_SILENT_AFTER` | `24h` | How long since `last_matched_at` (event ledger close time) before the monitors list marks a monitor silent. |
| `HTTP_ADDR` | `:8080` | Listen address for the API and dashboard. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` (structured JSON via `log/slog`). |
| `READYZ_LAG_THRESHOLD` | `0` (disabled) | Fail `/readyz` when poller ledger lag (chain tip minus last processed ledger) exceeds this. Unset or `0` leaves existing probes unchanged. |
| `ALERT_RETENTION` | unset (keep forever) | Age after which alerts are deleted in batches of 1000 (`90d`, `24h`, …). `delivery_attempts` follow via `ON DELETE CASCADE`. Unset preserves current behaviour: nothing is pruned. |

## Where secrets live

Channel secrets — webhook URLs, bot tokens, SMTP credentials — are stored in each channel's `config` JSON in the database, **not** in environment variables. SoroBeacon never logs them and never returns them from the API or renders them in the dashboard.

### Encrypting channel config at rest

Set `CONFIG_ENCRYPTION_KEY` to a base64-encoded AES-GCM key and SoroBeacon encrypts each channel's `config` before it is written and decrypts it transparently on read. Raw rows, backups and `pg_dump` output then hold an opaque JSON envelope (`{"sorobeacon_config":"v1:…"}`) instead of usable credentials.

```sh
openssl rand -base64 32
```

The key is a secret. Keep it with your other credentials (a systemd `EnvironmentFile=`, a secret manager, …) and **back it up next to your database backups**. A wrong length or non-base64 value fails startup, not the first channel write. With the key set, the startup log line reports `config_encryption_enabled=true`.

**Existing rows are not rewritten automatically.** Rows written before the key was set stay plaintext; they keep working and are re-encrypted lazily the next time the channel is updated (through the dashboard or the API). A read never fails just because a row is legacy plaintext, so enabling encryption on a running deployment does not brick it.

**Unset key ⇒ plaintext, with a warning.** With no key, behaviour is unchanged (config stored as plaintext) and SoroBeacon logs one warning at startup so the operator knows. This keeps upgrades safe by default.

**If the key is lost**, rows encrypted with it cannot be recovered — AES-GCM decryption is bound to the key. Reads fail with an error naming the channel (never echoing ciphertext or key material), which means the channel list is unavailable until the key is restored; the only recovery without the key is to delete and recreate the affected channels. **If the key is rotated**, keep the old key active while each channel is re-saved so its row is re-encrypted under the new one; swapping the key before rows are rewritten makes them undecryptable.

{% hint style="warning" %}
Encrypting config at rest only protects data at rest. Set `API_TOKEN` so the API and dashboard need a credential — with it unset both are open to anyone who can reach the port. Restrict database access either way.
{% endhint %}

## Behavior under errors

* **RPC failures** back off exponentially, capped at 10× the poll interval, then recover automatically.
* **Delivery failures** are retried 3 times per channel with exponential backoff (1s, 2s, 4s); every attempt is recorded in `delivery_attempts`.
* **Restarts** resume from the last checkpointed ledger; the `(rule_id, event_id)` dedup guard guarantees a replayed window never re-alerts.
