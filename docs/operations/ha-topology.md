# High-Availability (HA) Topology Guide

SoroBeacon combines two workloads with opposite scaling properties in a single binary:

1. **Stateless Control & Query Plane (`WORKLOAD_ROLE=api`)** — serves the REST API (`/api/v1`), GraphQL (`/graphql`), live alert streams (`/api/v1/alerts/stream`), and the web dashboard (`/`). Capacity scales horizontally with replica count.
2. **Leader-Gated Background Worker (`WORKLOAD_ROLE=worker`)** — runs the Stellar event poller, rule evaluator, notification dispatcher, digest flusher, escalation stepper, retention pruners, and external dead-man's-switch heartbeat. Exactly **one** instance holds the PostgreSQL session advisory lock (`0x534F4245434F4E` in [`internal/lease`](../../internal/lease/lease.go)) at any time.

Deploying both workloads under a single `Deployment` replica count forces an operator to choose between under-provisioning the API or running many idle advisory-lock contenders that add zero alerting throughput. Setting `worker.enabled=true` in the Helm chart (`deploy/helm/sorobeacon`) separates them into two independently managed `Deployment`s.

---

## What to Scale (and What Not to Scale)

| Workload | Helm Values | Recommended HA Setting | Why |
| --- | --- | --- | --- |
| **API (`<fullname>`)** | `replicaCount` or `autoscaling.*` | `3+` replicas (or HPA `minReplicas: 3`, `maxReplicas: 10`) | Every API pod runs with `--role=api` (`WORKLOAD_ROLE=api`), serves HTTP/gRPC traffic behind the `Service`, and **never** opens a PostgreSQL advisory-lock session (`lease.Disabled`). Adding replicas linearly increases read, write, and SSE fan-out capacity. |
| **Worker (`<fullname>-worker`)** | `worker.enabled: true`, `worker.replicaCount` | `2` replicas (`1` active leader + `1` warm standby) | Only the pod holding advisory lock `0x534F4245434F4E` runs the ingest and delivery loops. A second replica acts as a warm standby that promotes within `lease.DefaultInterval` (`3s`) when the leader drains or restarts. **Do not scale `worker.replicaCount` above `2` and never attach an HPA to the worker Deployment** — extra worker pods do not partition rule evaluation and only add lock-polling round trips. |

---

## Autoscaling Signal Selection (`autoscaling`)

Autoscaling is **off by default** (`autoscaling.enabled: false`) so the chart works out-of-the-box on clusters without `metrics-server` or a custom metrics adapter.

When enabled (`autoscaling.enabled: true`), the `HorizontalPodAutoscaler` targets only the API `Deployment`:

- **Why CPU alone is insufficient:** SoroBeacon's API is I/O-bound. Goroutines serving `/api/v1/alerts`, `/graphql`, or holding open `/api/v1/alerts/stream` SSE connections spend most of their wall time waiting on PostgreSQL queries or network I/O. Blocked goroutines consume memory (per-request JSON/GraphQL AST buffers and per-subscriber SSE channels) rather than CPU cycles.
- **Built-in resource signals:** The chart pairs **memory utilization (`targetMemoryUtilizationPercentage: 75`)** with **CPU utilization (`targetCPUUtilizationPercentage: 80`)** and a 300-second scale-down stabilization window (`behavior.scaleDown.stabilizationWindowSeconds: 300`) so open SSE streams are not churned by brief traffic dips.
- **Custom request-rate / stream metrics (`autoscaling.customMetrics`):** Clusters running Prometheus Adapter or KEDA can scale directly on in-flight HTTP request rate (`sorobeacon_http_requests_total`) without requiring any chart template edits:

```yaml
autoscaling:
  enabled: true
  minReplicas: 3
  maxReplicas: 12
  targetMemoryUtilizationPercentage: 75
  targetCPUUtilizationPercentage: 80
  customMetrics:
    - type: Pods
      pods:
        metric:
          name: sorobeacon_http_requests_per_second
        target:
          type: AverageValue
          averageValue: "100"
```

---

## Drain-Safe `PodDisruptionBudget`s (`podDisruptionBudget` & `worker.podDisruptionBudget`)

Each workload has its own optional `PodDisruptionBudget` (`templates/pdb.yaml`):

- **Multi-replica workloads (`replicas >= 2`):** Default to `minAvailable: 1`, ensuring voluntary evictions (`kubectl drain`, cluster upgrades) keep at least one API pod serving traffic and at least one worker pod online to hold or immediately acquire the leader lease.
- **Single-replica guard (`replicas <= 1`):** If a workload runs with `replicaCount: 1` (for example, a single-replica worker `worker.replicaCount: 1`), a PDB with `minAvailable: 1` or `maxUnavailable: 0` would block `kubectl drain` indefinitely because evicting the sole pod would reduce available replicas to `0`. The chart template automatically clamps any single-replica PDB to `maxUnavailable: 1` so node drains always succeed.

---

## Rolling Updates, Readiness Gating & Termination Grace Period

- **Zero-downtime rollouts:** Both `Deployment`s configure `strategy.rollingUpdate: { maxUnavailable: 0, maxSurge: 1 }` and `minReadySeconds: 5`. A replacement pod must pass `/api/v1/livez` (`startupProbe`) and `/api/v1/readyz` (`readinessProbe`) and stay ready for 5 seconds before Kubernetes terminates the old pod.
- **Fast leader hand-over (`<= 3s`):** When the active worker pod receives `SIGTERM`, `server.run` cancels the leader loop, which finishes the current poll/dispatch cycle and explicitly executes `pg_advisory_unlock` before closing its session. The warm standby worker replica acquires the lock on its next tick (`lease.DefaultInterval = 3s`), bounding alert-evaluation pause during a rollout to at most 3 seconds.
- **Termination grace period (`terminationGracePeriodSeconds: 60`):** `config.shutdownGracePeriod` defaults to `30s` (the maximum time the notification dispatcher waits for in-flight webhook/SMTP deliveries to complete before persisting them as retries), plus up to `10s` for the HTTP server drain and `5s` for explicit advisory-lock release. Setting `terminationGracePeriodSeconds: 60` gives the process a 15-second safety margin so `kubelet` never sends `SIGKILL` while deliveries are still draining.

---

## Resource Requests & Limits Basis

Both `resources` (API) and `worker.resources` (Worker) default to:

```yaml
requests:
  cpu: 100m
  memory: 128Mi
limits:
  cpu: 500m
  memory: 256Mi
```

- **Memory basis:** The statically compiled Go binary, embedded HTML templates/migrations, and `pgxpool` connections occupy ~35 MiB RSS at idle. For the API, `128Mi` request / `256Mi` limit accommodates ~500 concurrent `/alerts/stream` SSE subscribers and `1 MiB` (`HTTP_MAX_BODY_BYTES`) write payloads with >2x headroom. For the worker, `128Mi` / `256Mi` covers evaluating rules over 1,000-event poll pages and in-memory frequency/flapping windows.
- **CPU basis:** `100m` guarantees scheduling priority for TLS handshakes, rule evaluation, and webhook signing; `500m` allows burst throughput during historical CSV/NDJSON exports or ledger catch-up after an RPC outage.

---

## Migrations Across Replicas (`migrations.enabled` vs Startup Migration)

SoroBeacon guarantees that no pod serves traffic against an unmigrated database schema through two coordinated layers:

1. **Pre-install / pre-upgrade Helm hook (`migrations.enabled: true`):** Runs `/usr/local/bin/sorobeacon migrate` in a one-shot Kubernetes `Job` **before** either the API `Deployment` or the Worker `Deployment` is rolled. If a migration fails, the Helm upgrade aborts while the existing replicas continue serving on the previous release.
2. **Startup migration gate (`store.Migrate` before `/api/v1/livez`):** On startup, every pod runs `store.Migrate` before binding the HTTP listener. `golang-migrate` acquires a PostgreSQL advisory lock around `schema_migrations`:
   - When `migrations.enabled: true`, the hook `Job` has already applied all pending migrations, so each pod's startup check observes `ErrNoChange` (`current == latest`) and proceeds in milliseconds.
   - When `migrations.enabled: false`, the first replica to reach Postgres acquires the migration advisory lock and applies pending migrations while concurrent replicas wait on the lock and then observe `ErrNoChange`. Until `store.Migrate` and `store.MigrationState` succeed, `/api/v1/livez` (`startupProbe`, 150s window) does not answer, preventing `kubelet` from routing traffic to or restart-looping a migrating pod.
