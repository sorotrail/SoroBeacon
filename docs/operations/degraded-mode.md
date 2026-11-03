# Degraded Mode and Database Outage Handling

When the underlying database becomes unreachable, SoroBeacon operates in a designed **degraded mode** rather than failing unexpectedly or hanging client requests.

---

## 1. What Answers and What Fails

| Endpoint Class | Examples | Degraded Behavior | Status Code |
| :--- | :--- | :--- | :--- |
| **Liveness Probe** | `GET /livez` | Answers normally (`{"status":"alive"}`). The process is healthy and must not be restarted by orchestrators. | `200 OK` |
| **Version & Metadata** | `GET /version` | Answers normally from in-memory build metadata. | `200 OK` |
| **Readiness Probe** | `GET /readyz` | Reports database unhealthy (`status: "not ready"`). Load balancers take the instance out of rotation immediately. | `503 Service Unavailable` |
| **Store-Dependent APIs** | `GET /monitors`, `POST /monitors`, `GET /alerts`, `GET /channels`, `GET /stats`, etc. | **Fails fast** through the circuit breaker without querying the database. Responds with `Retry-After: 5` header and `SERVICE_UNAVAILABLE` error code. | `503 Service Unavailable` |

---

## 2. Fast Failure via Circuit Breaking

A circuit breaker (`StoreBreaker`) guards all database access.
- When consecutive unavailable failures reach the limit (default: 3), the breaker trips to `open`.
- In `open` state, incoming API requests fail fast immediately at the middleware layer. No queries are issued to the database connection pool, preventing connection starvation and request queuing.
- Once the recovery cooldown elapses, the breaker transitions to `half-open` and permits a single probe to test connectivity.
- A successful probe closes the breaker and resumes traffic. A failed probe re-trips the breaker with exponential backoff.

---

## 3. Stampede Protection via Jittered Backoff

When a database recovers from an outage, reconnecting replicas and retrying clients can cause a secondary outage stampede. SoroBeacon mitigates this by:
- Adding **jitter** to probe delays and reconnection backoffs across replicas.
- Poller loops back off exponentially with randomized jitter (`rand.Int63n`) before retrying.
- The poller holds contract and ledger cursors at the last confirmed position without advancing past unpersisted alerts.

---

## 4. Operator Alerting & Dead-Man Signal

When the database is unreachable, SoroBeacon cannot persist or deliver alerts through its normal delivery channels. Detecting and alerting on this state is the **operator's responsibility**:
- **Metric Signal**: SoroBeacon exports `sorobeacon_database_degraded` (gauge: `1` when degraded, `0` when healthy) and `sorobeacon_database_unavailable_total` (counter).
- **Dead-Man Switch**: Configure Prometheus or an external monitoring system (e.g. Healthchecks.io, Cronitor, PagerDuty Dead Man's Snitch) to watch `sorobeacon_database_degraded > 0` or missing `/readyz` probes.
- **Log Rate-Limiting**: SoroBeacon logs database outage transitions once when entering and once when recovering from degraded mode. Individual request failures do not emit repetitive error logs to prevent log flooding.
