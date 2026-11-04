# baseline\_deviation

Fires when a numeric value extracted from a matching contract event **deviates from its learned rolling baseline** by at least `sensitivity` multiples of the baseline spread. Where [`value_threshold`](value-threshold.md) requires an operator to know the exact static cutoff in advance, `baseline_deviation` learns normal contract behaviour over a rolling window and alerts on unexpected spikes or drops.

## Params

```json
{
  "event_name": "transfer",
  "value_path": "amount",
  "window": "24h",
  "warmup": "10m",
  "min_samples": 5,
  "sensitivity": 3,
  "direction": "both",
  "seasonality": "hour_of_day"
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `window` | yes | Rolling history window as a Go duration (`"1h"`, `"24h"`, `"168h"`). Observations older than `window` are evicted before computing the baseline. |
| `sensitivity` | yes | Positive multiplier of the baseline spread (`> 0`). A sensitivity of `3` fires when `\|observed - baseline\| >= 3 * spread`. |
| `event_name` | no | Only evaluate events with this name (first topic). Omit to evaluate every event carrying a numeric value. |
| `value_path` | no | Dot path into the decoded event fields or value (e.g. `"amount"`). Omit when the event value itself is a number. |
| `warmup` | no | Initial learning period from the rule's first observation (or last rule edit) during which the rule collects observations without firing (default `"10m"` in the dashboard builder, `"0s"` when omitted in raw JSON). |
| `min_samples` | no | Minimum observations required in the active seasonality bucket before the rule can fire (default `5`). |
| `direction` | no | `"both"` (default), `"above"` (alert only on upward spikes), or `"below"` (alert only on downward drops). |
| `seasonality` | no | `"none"` (default), `"hour_of_day"` (24 UTC hourly buckets), or `"day_of_week"` (7 UTC weekday buckets). |
| `cooldown` | no | Cross-cutting repeat-alert suppression window. See [Rule cooldown](cooldown.md). |

## Statistical method and explainability

A baseline rule is only useful if an operator paged at 03:00 can see *why* it fired. Instead of black-box smoothing, `baseline_deviation` uses a **spike-resistant median and Median Absolute Deviation (MAD)** over a bounded sliding window (up to `256` recent observations per bucket, alongside an incremental Welford running mean):

1. **Baseline center (`baseline`)**: the median of observations inside `window` for the active seasonality bucket.
2. **Baseline spread (`spread`)**: `1.4826 * median(|x_i - baseline|)` (the normal-consistent MAD scale), floored at `1%` of `|baseline|` so tiny floating-point jitter does not produce artificial alarms.
3. **Degenerate / constant history**: When all historical observations are identical (`MAD == 0`, e.g. five identical `100` transfers), `spread` falls back to `max(|baseline|, 1.0)` so `sensitivity: 3` requires a `3x` departure from the constant level rather than dividing by zero. Because the center and spread are median-based, a single huge outlier during warm-up cannot corrupt the baseline or mask subsequent anomalies.
4. **Anomaly isolation**: Normal observations are folded into the rolling window; observations that trigger an anomaly alert are not added to the baseline, so an ongoing incident does not teach the rule that the incident is normal.

Calling `Explain` (or inspecting `LastDecision`) returns the exact `baseline`, `mean`, `spread`, `observed`, `deviation` multiplier, `sample_count`, `warmup_remaining`, and `seasonality_bucket`.

## Maintenance windows and suppression

Events that arrive while a monitor or contract is under an active maintenance window or suppression (`WithBaselineSuppressed` / `WithSuppressionCheck`) neither fire the rule nor enter the rolling baseline. An outage or planned migration during a maintenance window therefore leaves the pre-maintenance baseline intact.

## Seasonality and retention limits

Contract traffic often follows daily or weekly cycles. Setting `seasonality: "hour_of_day"` maintains 24 independent UTC hour buckets so weekday morning volume is compared against prior 09:00 UTC observations rather than 02:00 UTC quiet periods; `"day_of_week"` maintains 7 weekday buckets.

**Limitations:** Each bucket retains at most `256` recent observations in memory (keyed per rule and reset on rule edits). Long-horizon seasonal baselines spanning weeks of high-frequency traffic beyond the in-memory window would require pre-aggregated hourly rollups in the backing store.
