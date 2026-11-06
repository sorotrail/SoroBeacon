---
page_title: "sorobeacon_inhibition Resource - sorobeacon"
subcategory: ""
description: |-
  Manages an alert inhibition relationship between two rules.
---

# sorobeacon_inhibition (Resource)

Manages an alert inhibition relationship between two rules (`POST /inhibitions`).

## Example Usage

```hcl
resource "sorobeacon_inhibition" "pause_suppresses_transfer" {
  source_rule_id        = sorobeacon_rule.contract_paused.id
  target_rule_id        = sorobeacon_rule.transfer.id
  firing_window_seconds = 600
}
```

## Schema

### Required

- `source_rule_id` (Number) Rule ID whose recent firing suppresses alerts from `target_rule_id`. Changing this forces replacement.
- `target_rule_id` (Number) Rule ID suppressed while `source_rule_id` is firing. Changing this forces replacement.

### Optional

- `firing_window_seconds` (Number) Lookback window in seconds within which a source rule firing suppresses the target rule. Defaults to `600`. Changing this forces replacement.

### Read-Only

- `id` (String) Composite identifier `<source_rule_id>/<target_rule_id>`.

## Import

```sh
terraform import sorobeacon_inhibition.pause_suppresses_transfer 10/11
```
