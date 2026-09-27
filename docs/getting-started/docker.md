# Running SoroBeacon with Docker Compose

SoroBeacon can be run entirely with Docker Compose, including a Postgres database
for persistence. This is the recommended way to deploy SoroBeacon in production
or for local development.

## Quick start

```sh
docker compose up --build -d
```

The above command starts SoroBeacon and a Postgres instance against **Stellar testnet**.
The dashboard will be available at [http://localhost:8080](http://localhost:8080)
and the API at [http://localhost:8080/api/v1](http://localhost:8080/api/v1).

Verify it's healthy:

```sh
curl -s localhost:8080/api/v1/health
# {"db":"ok","rpc":"ok","rpc_latest_ledger":...,"status":"ok"}
```

## What gets started

The compose file defines two services:

| Service | Description |
| --- | --- |
| **`sorobeacon`** | The SoroBeacon application, listening on port 8080 |
| **`postgres`** | Postgres 16-Alpine, containing the SoroBeacon database |

The `sorobeacon` service depends on `postgres` and waits for it to be healthy before
starting. The `postgres` service has its own healthcheck.

## Configuration via environment variables

All configuration is passed to SoroBeacon as environment variables. The compose
file sets sensible defaults, but you can override them in your own `.env` file or
via the `docker compose up` `-e` flag.

The following variables are configured by default in `docker-compose.yml`:

| Variable | Default | What it does |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://sorobeacon:sorobeacon@postgres:5432/sorobeacon?sslmode=disable` | Connection to the Postgres service |
| `RPC_URL` | `https://soroban-testnet.stellar.org` | Stellar RPC endpoint for event polling |
| `POLL_INTERVAL` | `5s` | How often the poller queries the RPC |
| `HTTP_ADDR` | `:8080` | Listen address for the API and dashboard |
| `LOG_LEVEL` | `info` | Minimum structured log level |

To override any of these, add them to a `.env` file in the same directory as
`docker-compose.yml`, or pass them on the command line:

```sh
docker compose up --build -e RPC_URL=https://my-mainnet-rpc.example.com -e LOG_LEVEL=debug
```

For a full list of all environment variables, see
[`configuration.md`](configuration.md). That page contains the complete
operator table including network presets, database pool tuning, rate limiting,
and more — we refer to it rather than duplicate the information here.

## Database persistence

The Postgres service uses a **named volume** called `pgdata` to persist data:

```yaml
volumes:
  pgdata:
```

Without this volume, any alerts, monitors, or configuration entered into SoroBeacon
would be lost when the container stops. With the volume, data persists across
restarts and across `docker compose down`/`up` cycles.

To explicitly **drop** the volume (for example, to start fresh), run:

```sh
docker compose down -v
```

**Warning:** `docker compose down -v` permanently deletes the Postgres data
directory and all stored alerts/monitors.

## Container healthcheck

Both services expose a healthcheck:

### SoroBeacon (`sorobeacon`)

```yaml
healthcheck:
  test: ["CMD", "wget", "-q", "-O-", "http://localhost:8080/api/v1/health"]
  interval: 10s
  timeout: 3s
  start_period: 5s
  retries: 3
```

This checks the `/api/v1/health` endpoint, which reports the status of both
the Postgres connection and the RPC endpoint. It is considered healthy only when
both are OK.

### Postgres (`postgres`)

```yaml
healthcheck:
  test: ["CMD-SHELL", "pg_isready -U sorobeacon"]
  interval: 2s
  timeout: 2s
  retries: 15
```

This runs `pg_isready` to verify the Postgres process is accepting connections.
The `depends_on: condition: service_healthy` on the sorobeacon service ensures
SoroBeacon won't start until Postgres passes this check.

## Following logs

View real-time logs from both services:

```sh
docker compose logs -f
```

To see only SoroBeacon logs:

```sh
docker compose logs -f sorobeacon
```

To see only Postgres logs:

```sh
docker compose logs -f postgres
```

## Custom networks and external databases

If you need to connect SoroBeacon to an existing Postgres instance, set
`DATABASE_URL` to point at your external host and omit the `postgres` service
from `docker-compose.yml`. You can still run the SoroBeacon service alone,
passing `DATABASE_URL` via `-e` or `.env`.

For production deployments, consider placing SoroBeacon behind a reverse proxy
that handles TLS termination and provides basic API authentication — the MVP
has no internal authentication.