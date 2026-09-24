# SoroBeacon Helm chart

Deploys SoroBeacon — the poller, the JSON API and the dashboard, all in one
process — to Kubernetes.

```sh
helm install sorobeacon deploy/helm/sorobeacon \
  --namespace sorobeacon --create-namespace \
  --set database.url='postgres://sorobeacon:…@postgres.example:5432/sorobeacon?sslmode=disable' \
  --set auth.apiToken="$(openssl rand -hex 32)"
```

The chart requires an **external** PostgreSQL instance. It deliberately does
not bundle one, so backups, high availability and version upgrades stay yours
to manage ([CloudNativePG](https://cloudnative-pg.io) and the
[Zalando operator](https://github.com/zalando/postgres-operator) both work
well). Migrations are handled by the application itself — see
[Migrations](#migrations) below.

## What it creates

| Resource | Notes |
| --- | --- |
| `Deployment` | The SoroBeacon process. Probes, resources, security context. |
| `Service` | ClusterIP in front of the HTTP port (API + dashboard + `/metrics`). |
| `ConfigMap` | Every non-secret environment variable, so the effective configuration is readable with `kubectl describe configmap`. |
| `Secret` | `DATABASE_URL`, and `API_TOKEN` / `CONFIG_ENCRYPTION_KEY` when set. Skipped in favour of your own Secret when `database.existingSecret` / `auth.existingSecret` is set. |
| `ServiceAccount` | Optional; SoroBeacon calls no Kubernetes API, so it has no RBAC. |
| `Job` (optional) | Migration hook, `migrations.enabled=true`. |

## Configuration

Every environment variable `internal/config` reads has a key in
[`values.yaml`](values.yaml) carrying the same default the process uses when
the variable is unset, so an install with no overrides behaves exactly like
running the binary with an empty environment. The operator reference for what
each variable does is [`docs/configuration.md`](../../../docs/configuration.md).

| values.yaml | Environment variable |
| --- | --- |
| `config.network` | `NETWORK` |
| `config.rpcUrl` | `RPC_URL` |
| `config.networkPassphrase` | `NETWORK_PASSPHRASE` |
| `config.sourceMode` | `SOURCE_MODE` |
| `config.sorotrailUrl` | `SOROTRAIL_URL` |
| `config.httpAddr` | `HTTP_ADDR` |
| `config.httpMaxBodyBytes` | `HTTP_MAX_BODY_BYTES` |
| `config.pollInterval` | `POLL_INTERVAL` |
| `config.logLevel` | `LOG_LEVEL` |
| `config.monitorSilentAfter` | `MONITOR_SILENT_AFTER` |
| `config.readyzLagThreshold` | `READYZ_LAG_THRESHOLD` |
| `config.rateLimitRps` | `RATE_LIMIT_RPS` |
| `config.rateLimitBurst` | `RATE_LIMIT_BURST` |
| `config.rateLimitTrustForwarded` | `RATE_LIMIT_TRUST_FORWARDED` |
| `config.corsAllowedOrigins` | `CORS_ALLOWED_ORIGINS` |
| `config.alertRetention` | `ALERT_RETENTION` |
| `database.maxConns` | `DATABASE_MAX_CONNS` |
| `database.minConns` | `DATABASE_MIN_CONNS` |
| `database.maxConnLifetime` | `DATABASE_MAX_CONN_LIFETIME` |
| `database.maxConnIdleTime` | `DATABASE_MAX_CONN_IDLE_TIME` |
| `database.url` / `database.existingSecret` | `DATABASE_URL` (Secret) |
| `auth.apiToken` / `auth.existingSecret` | `API_TOKEN` (Secret) |
| `auth.configEncryptionKey` / `auth.existingSecret` | `CONFIG_ENCRYPTION_KEY` (Secret) |

`config.httpAddr` must match `service.targetPort`: the probes and the Service
both address the container port, so a mismatch leaves every pod unready.

### Secrets

Three values are credentials and never appear in the ConfigMap:

- `DATABASE_URL` — the connection string usually embeds a password.
- `API_TOKEN` — anyone holding it can read and mutate everything the API
  exposes.
- `CONFIG_ENCRYPTION_KEY` — the AES-GCM key that decrypts each channel's
  `config`, which holds webhook URLs, bot tokens and SMTP credentials.

Each can come from the chart-managed Secret or from one you already have:

```yaml
database:
  existingSecret: sorobeacon-db        # contains DATABASE_URL
  existingSecretKey: DATABASE_URL
auth:
  existingSecret: sorobeacon-auth      # contains API_TOKEN and CONFIG_ENCRYPTION_KEY
```

Use the external form when credentials come from External Secrets, Sealed
Secrets or a manually created object: rotating them then never touches a Helm
release. A Secret referenced this way must contain **every** key the deployment
references — `auth.existingSecret` needs both `API_TOKEN` and
`CONFIG_ENCRYPTION_KEY` unless the corresponding key name is empty, and a
missing key leaves the pod in `CreateContainerConfigError`.

`CONFIG_ENCRYPTION_KEY` cannot be rotated like `API_TOKEN`: it is the key that
decrypts stored channel configs, so losing it makes those rows
undecryptable. Back it up with the database.

## Migrations

**The application applies its own migrations, during startup and before it binds
the HTTP listener.** `store.Migrate` runs ahead of `ListenAndServe`, so no pod
can serve traffic against a schema it has not migrated, and `/api/v1/livez`
cannot answer until that has happened. Concurrent replicas are serialised by
golang-migrate's Postgres advisory lock, so a rolling update that starts several
pods at once is safe.

That ordering is why the chart needs no init container, and why the Deployment
carries a **startup probe**: a migration that takes longer than the liveness
probe's tolerance must not restart the pod that is running it. The startup probe
allows a 150s window (`periodSeconds: 5`, `failureThreshold: 30`).

`migrations.enabled=true` adds an **optional pre-install/pre-upgrade hook Job**
that runs the same migration path once, before the Deployment is rolled, so a
broken migration fails the release while the previous version is still serving
instead of crash-looping the new pods. It is off by default because the in-pod
migration already runs before traffic, and the hook costs one short-lived pod
per release.

Two details worth knowing about the hook:

- **Why a hook and not an init container.** SoroBeacon has no migrate-only mode:
  the binary always starts the server after migrating. An init container would
  therefore have to start the whole process, wait for it, and kill it — once per
  pod per replica — duplicating work the main container already does. A
  pre-upgrade hook does it once per release.
- **Why the hook ships its own ConfigMap and Secret.** Helm runs `pre-install`
  hooks *before* the rest of the release is created, so a hook pod that mounted
  the release's ConfigMap or Secret would never start. The hook's copies are
  rendered from the same helpers, so they cannot drift from the released values.

The hook sets `POLL_INTERVAL=24h` for its own lifetime: it exists only to
migrate, and this keeps the short-lived process from ingesting events or
creating alerts on the way past.

Because the new pods migrate while the old ones still serve, **a migration must
stay backwards-compatible for one release** — drop a column only after the
release that stopped writing to it.

## Probes

| Probe | Endpoint | Why |
| --- | --- | --- |
| `startupProbe` | `/api/v1/livez` | Covers the migration window; livez only answers once the listener is bound. |
| `livenessProbe` | `/api/v1/livez` | Liveness checks the process only, deliberately: a database or RPC outage must not restart-loop the pod. |
| `readinessProbe` | `/api/v1/readyz` | Checks the database and the event source, so an unreachable dependency takes the pod out of the Service without killing it. |

All three are exempt from `API_TOKEN`, so an authenticated deployment cannot
fail its own health checks.

## Deploying

```sh
helm upgrade --install sorobeacon deploy/helm/sorobeacon \
  --namespace sorobeacon --create-namespace -f production.yaml
```

with a `production.yaml` such as:

```yaml
replicaCount: 3
config:
  network: mainnet
  rpcUrl: https://your-provider.example/soroban
  logLevel: warn
  alertRetention: 90d
  rateLimitRps: 10
database:
  existingSecret: sorobeacon-db
auth:
  existingSecret: sorobeacon-auth
migrations:
  enabled: true
resources:
  requests: { cpu: 250m, memory: 256Mi }
  limits:   { cpu: 1000m, memory: 512Mi }
```

`values.schema.json` fails the release on structural mistakes — a non-numeric
`replicaCount`, or a `database` block with neither `url` nor `existingSecret`.

The Service exposes `/metrics` on the same port as the API; scrape it from
inside the cluster (it is unauthenticated, so do not publish the port to the
internet).

## Tests

`render_test.go` renders the chart with `helm template` and asserts the
structural invariants this chart promises — probes on the real endpoints,
`DATABASE_URL` always from a Secret, the migration hook's pod never matching the
Service selector, and every environment variable `internal/config` reads being
reachable through `values.yaml`. The env-var list is read from the Go source, so
adding a variable there without adding a values key here fails the test. The
tests skip when `helm` is not installed.
