# Cross-Workspace Notification Delivery Fairness

SoroBeacon isolates notification delivery across workspaces and channels so a stalled webhook, high-volume alert burst, or retry storm in one workspace never starves or delays deliveries belonging to other workspaces.

## Fairness Policy & Architecture

Notification delivery is governed by `notify.FairnessScheduler` (`internal/notify/fairness.go`), which enforces three orthogonal bounds:

1. **Per-Workspace Concurrency & Queue Bounds (`workspaceSlotPool`)**
   - Each workspace has an independent slot pool partitioned into two lanes:
     - **First-attempt lane (`first_attempt`):** Up to `MaxWorkspaceInFlight` (default `8`) concurrent first-attempt deliveries and `MaxWorkspaceQueue` (default `64`) queued deliveries.
     - **Retry lane (`retry`):** Up to `MaxWorkspaceRetryInFlight` (default `2`) concurrent retry attempts (`attempt >= 2`) and `MaxWorkspaceRetryQueue` (default `16`) queued retry attempts.
   - Multi-workspace fan-out in flat dispatch (`dispatchFlat`), tiered escalations (`deliverStep` and `ProcessDueEscalations`), and window digests (`FlushDigests`) executes concurrently across workspaces so a slow destination in Workspace A never blocks Workspace B.

2. **Per-Channel Concurrency & Head-of-Line Ordering (`channelSlotPool`)**
   - Each channel has an independent `channelGate` with `MaxChannelInFlight` (default `1`) and `MaxChannelQueue` (default `32`).
   - With `MaxChannelInFlight = 1`, deliveries to a single channel execute sequentially in strict FIFO order (`firstWaiters`), preserving per-channel head-of-line ordering while bounding goroutine and memory growth when an endpoint hangs.
   - If both a fresh first-attempt delivery and a retry attempt are waiting on the same channel gate, first-attempt waiters are awakened before retry waiters.

3. **Retry-Storm Containment**
   - Workspace and channel slots are held **only** for the duration of the active `Notifier.Send` call (`sendWithRecovery`) and are released immediately before any exponential backoff sleep (`time.After(backoff)`).
   - When an alert wakes from backoff for `attempt >= 2`, it must acquire a slot from the workspace's dedicated `retry` lane (`MaxWorkspaceRetryInFlight`), leaving `MaxWorkspaceInFlight` capacity available for first-attempt deliveries.

4. **Non-Blocking Ingestion (`Dispatcher.Submit` / `AsyncDispatch`)**
   - Callers can enqueue alerts non-blockingly via `Dispatcher.Submit(ctx, alert)` (or configure `FairnessConfig{AsyncDispatch: true}`).
   - Each workspace drains its bounded submission queue using at most `MaxWorkspaceInFlight` per-workspace workers that exit automatically when idle, ensuring rule evaluation and `Broadcaster.Publish` never block on downstream HTTP/SMTP I/O.

## Backpressure & Dead-Letter Pressure Release

When a workspace or channel exceeds its configured queue bound (`ErrWorkspaceQueueFull`, `ErrRetryQueueFull`, or `ErrChannelQueueFull`), the excess delivery fails immediately without blocking:

- A failed `DeliveryAttempt` row is recorded via `RecordDeliveryAttempt` (with sanitized error text; channel config secrets are never logged or persisted).
- Channel health is updated via `RecordChannelHealth` (honoring `CHANNEL_DISABLE_AFTER_FAILURES` auto-disable thresholds).
- A `DeadLetter` row is created via `CreateDeadLetter` so operators can inspect and redrive shed alerts once the downstream receiver recovers.

## Prometheus Metrics

All workspace labels are hashed via `metrics.HashWorkspaceLabel` (`ws_<12-hex>`) and capped at `MaxTrackedWorkspaces` (`256`) to bound cardinality:

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `sorobeacon_notify_workspace_in_flight` | Gauge | `network`, `workspace`, `lane` | Current in-flight delivery attempts for a workspace (`first_attempt` or `retry`). |
| `sorobeacon_notify_workspace_queue_depth` | Gauge | `network`, `workspace`, `lane` | Current queued delivery attempts waiting for a workspace slot. |
| `sorobeacon_notify_channel_in_flight` | Gauge | `network`, `channel_id` | Current in-flight delivery attempts for a channel. |
| `sorobeacon_notify_channel_queue_depth` | Gauge | `network`, `channel_id` | Current delivery attempts queued behind an active delivery on a channel. |
| `sorobeacon_notify_fairness_overflow_total` | Counter | `network`, `workspace`, `reason` | Total deliveries shed to dead letters due to queue saturation (`workspace_queue_full`, `retry_queue_full`, `channel_queue_full`). |

## Before / After Contention Behavior

| Scenario | Before (Shared Sequential Fan-Out) | After (`FairnessScheduler`) |
| --- | --- | --- |
| Stalled webhook (`10s` timeout, `3` attempts) in Workspace A while Workspace B emits 5 alerts | Workspace B waits up to `37s` (`3 * 10s + 1s + 2s + 4s` backoff) behind Workspace A | Workspace B delivers all 5 alerts in `< 5ms` while Workspace A is blocked |
| Burst of `100` alerts in noisy Workspace A (`MaxWorkspaceInFlight=2, MaxWorkspaceQueue=3`) | Unbounded queueing or head-of-line blocking across all tenants | `5` admitted (`2` active + `3` queued), `95` immediately shed to dead letters; quiet workspaces unaffected |
| Retry storm (`5` failing alerts retrying with backoff) alongside new alerts | Retries hold worker capacity during backoff and starve fresh alerts | Slots released during backoff; retries capped by `MaxWorkspaceRetryInFlight`, fresh alerts deliver in `< 5ms` |
