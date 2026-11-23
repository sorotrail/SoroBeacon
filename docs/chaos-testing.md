# Chaos & Composed Fault-Injection Testing

SoroBeacon's notification and ingestion pipeline is verified under composed multi-layer faults by the chaos harness in [`internal/notify/faultinject/chaos.go`](../internal/notify/faultinject/chaos.go) and [`internal/notify/faultinject/chaos_test.go`](../internal/notify/faultinject/chaos_test.go).

While single-fault tests in `faultinject_test.go` verify individual HTTP transport failures (`ConnectionReset`, `SlowBody`, `TruncatedBody`, `RateLimit`, `ServerError5xx`, `TLSFailure`, `DNSFailure`, `Hang`), the chaos suite composes faults across **seven subsystems simultaneously** under a deterministic PRNG seed and checks universal safety invariants after every run.

---

## Composed Fault Domains

Each chaos seed exercises all seven fault domains in an order and combination derived from the seed:

| Domain | Identifier | Injected Faults |
|---|---|---|
| **1. Channel HTTP Delivery** | `channel_delivery` | `FaultInjectingServer` HTTP `503`, `429` (`Retry-After`), truncated response bodies, slow response bodies, and hijacked TCP connection resets |
| **2. Database Store** | `database` | Transient connection resets before delivery-receipt commit and slow-query reconnects during poll cycles |
| **3. Leader Lease Coordination** | `leader_lease` | Leader lease expiry mid-poll with epoch increment and handover to a new poller instance (`poller-node-N`) re-reading the active ledger |
| **4. Stellar Soroban RPC** | `stellar_rpc` | Transient `GetEvents` HTTP 503 responses and `GetLatestLedger` timeouts requiring backoff and cursor retry |
| **5. Alert Escalation Mid-Flight** | `escalation_mid_flight` | Multi-step escalation ticks interleaved with concurrent operator `AcknowledgeAlert` actions |
| **6. Digest Queue Flush** | `digest_flush` | Upstream HTTP failures during scheduled `pending_digests` batch flushes, retaining queued alerts until a subsequent flush succeeds |
| **7. Process Restart Mid-Batch** | `process_restart` | Worker crash after persisting an alert to the store but before delivery, resuming from store checkpoints on restart |

---

## Universal Invariants Checked on Every Run

At the end of every run, `(*ChaosResult).CheckInvariants()` verifies five universal properties:

1. **Terminal Delivery or Dead-Letter (`terminal_delivery_or_dead_letter`):**
   Every detected alert is eventually delivered or recorded in `dead_letters`, and no digest alerts remain stranded in `pending_digests` after drain.
2. **Idempotent / Deduplicated Delivery (`no_duplicate_delivery_without_dedup_key`):**
   No alert is ever delivered twice to the same channel and step without a distinct deduplication key, and a committed deduplication key never produces a duplicate delivery receipt across retries, leader handovers, or process restarts.
3. **Acknowledgement Stops Escalation (`acknowledged_alert_never_escalates`):**
   Once an alert has a non-nil `acknowledged_at` timestamp, no escalation delivery may occur at or after `acknowledged_at`, even if an escalation step was already queued when the operator acknowledged the alert.
4. **Workspace Isolation (`workspace_isolation_holds`):**
   Every delivery receipt and dead-letter record remains strictly within the `workspace_id` of the monitor and alert that emitted it; zero cross-tenant leakage is tolerated across multi-workspace runs.
5. **Channel Secret Redaction (`channel_config_never_leaked`):**
   Sensitive credentials inside `Channel.Config` (webhook URLs, signing secrets, bearer tokens) never appear in structured log output, fault descriptions, or dead-letter error snippets.

---

## Reproducing a Failing Seed Locally

The chaos runner logs the seed and exact replay command **before** injecting any faults (`msg=chaos_suite_starting seed=<seed> replay_cmd=...`), ensuring that even a panic or test timeout leaves the seed visible in CI logs.

When an invariant fails, `ChaosFailureError` prints:
- The failing `seed`
- The one-line `replay_cmd`
- Every `broken_invariant` with the offending `alert_id`, `channel_id`, and `workspace_id`
- The full ordered `fault_sequence` leading up to the failure

To reproduce any seed locally:

```bash
SOROBEACON_CHAOS_SEED=611 go test -race -v -run TestChaosSuiteReplay ./internal/notify/faultinject
```

---

## Running a Scheduled / Soak Chaos Sweep

To run a time-bounded chaos sweep across newly generated seeds (for example in a nightly or weekly scheduled CI job or local soak test), set `SOROBEACON_CHAOS_DURATION`:

```bash
SOROBEACON_CHAOS_DURATION=2m go test -race -v -run TestChaosSuiteReplay ./internal/notify/faultinject
```

Each iteration logs `scheduled_chaos_seed_starting run=<N> seed=<seed> replay_cmd=...` before starting so any failure can be replayed immediately with `SOROBEACON_CHAOS_SEED=<seed>`.

---

## Promoting a Discovered Seed to the Regression Suite

When a scheduled run discovers a seed that exposes a bug:

1. Replay the failure locally with `SOROBEACON_CHAOS_SEED=<seed> go test -race -v -run TestChaosSuiteReplay ./internal/notify/faultinject`.
2. Fix the underlying bug while keeping the seed unchanged.
3. Add `<seed>` to the `regressionSeeds` slice in `TestChaosSuiteRegressionSeeds` inside [`internal/notify/faultinject/chaos_test.go`](../internal/notify/faultinject/chaos_test.go) so every future PR runs that exact fault sequence in `go test -race ./...`.
