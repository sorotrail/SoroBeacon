---
page_title: "sorobeacon_escalation_policy Resource - sorobeacon"
subcategory: ""
description: |-
  Manages a tiered escalation policy attached to a SoroBeacon monitor.
---

# sorobeacon_escalation_policy (Resource)

Manages a tiered escalation policy attached to a SoroBeacon monitor (`PUT /monitors/{id}/escalation`).

## Example Usage

```hcl
resource "sorobeacon_escalation_policy" "payments" {
  monitor_id = sorobeacon_monitor.payments.id

  steps = [
    {
      delay       = "0s"
      channel_ids = [sorobeacon_channel.slack.id]
    },
    {
      delay       = "5m0s"
      channel_ids = [sorobeacon_channel.pagerduty.id]
    },
  ]
}
```

## Schema

### Required

- `monitor_id` (Number) ID of the monitor this escalation policy belongs to. Changing this forces replacement.
- `steps` (Attributes List) Ordered list of escalation steps. (see [below for nested schema](#nestedatt--steps))

### Read-Only

- `id` (Number) Escalation policy ID assigned by the server.

<a id="nestedatt--steps"></a>
### Nested Schema for `steps`

Required:

- `channel_ids` (List of Number) Notification channel IDs to notify at this step.

Optional:

- `delay` (String) Go duration string before this step fires (e.g. `0s`, `5m0s`). Defaults to `0s`.

## Import

Import by monitor ID:

```sh
terraform import sorobeacon_escalation_policy.payments 42
```
