---
page_title: "sorobeacon_maintenance_window Resource - sorobeacon"
subcategory: ""
description: |-
  Manages a SoroBeacon maintenance window silencing alert deliveries during planned maintenance.
---

# sorobeacon_maintenance_window (Resource)

Manages a SoroBeacon maintenance window silencing alert deliveries during planned maintenance.

## Example Usage

```hcl
resource "sorobeacon_maintenance_window" "deploy" {
  reason     = "Scheduled contract upgrade"
  scope      = "monitor"
  monitor_id = sorobeacon_monitor.payments.id
  start_at   = "2026-11-10T02:00:00Z"
  end_at     = "2026-11-10T04:00:00Z"
}
```

## Schema

### Required

- `end_at` (String) Window end timestamp in RFC3339 format (UTC); must be after `start_at`.
- `reason` (String) Human-readable reason for the maintenance window.
- `scope` (String) Maintenance scope: `global`, `monitor`, or `contract`.
- `start_at` (String) Window start timestamp in RFC3339 format (UTC).

### Optional

- `contract_id` (String) Target Soroban contract ID when `scope` is `contract`.
- `monitor_id` (Number) Target monitor ID when `scope` is `monitor`.

### Read-Only

- `id` (Number) The ID of this resource.

## Import

```sh
terraform import sorobeacon_maintenance_window.deploy 12
```
