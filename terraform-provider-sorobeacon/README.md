# Terraform Provider for SoroBeacon

Manage SoroBeacon monitors, rules and channels as infrastructure-as-code.

## ⚠️ Channel secrets and Terraform state

Channel config contains credentials: webhook URLs, bot tokens, SMTP passwords.
**Terraform stores attribute values in plain text in state.** The `config`
attribute is marked `Sensitive` so `terraform plan` redacts it in output, but
the state file on disk contains the raw value.

**You must encrypt your state backend.** Use S3 with KMS, GCS with CMEK,
`terraform_remote_state` with encryption, or equivalent. If you use local
state, encrypt the disk.

## Usage

```hcl
terraform {
  required_providers {
    sorobeacon = {
      source = "sorotrail/sorobeacon"
    }
  }
}

provider "sorobeacon" {
  endpoint = "http://localhost:8080/api/v1"
  token    = var.sorobeacon_token
}

resource "sorobeacon_monitor" "payments" {
  name         = "Payment events"
  contract_ids = ["CDLZFC..."]
  enabled      = true
  channel_ids  = [sorobeacon_channel.slack.id]
}

resource "sorobeacon_rule" "transfer" {
  monitor_id = sorobeacon_monitor.payments.id
  type       = "event_emitted"
  params     = jsonencode({ topic = "transfer" })
}

resource "sorobeacon_channel" "slack" {
  name    = "ops-alerts"
  type    = "slack"
  config  = jsonencode({ webhook_url = var.slack_webhook_url })
}
```

## Import

All resources support `terraform import`:

```sh
terraform import sorobeacon_monitor.payments 42
terraform import sorobeacon_channel.slack 7
terraform import sorobeacon_rule.transfer 42/15   # monitor_id/rule_id
```

## Resources

| Resource                | Description                  |
|-------------------------|------------------------------|
| `sorobeacon_monitor`    | A contract event monitor     |
| `sorobeacon_rule`       | A rule on a monitor          |
| `sorobeacon_channel`    | A notification channel       |

## Data Sources

| Data Source             | Description                  |
|-------------------------|------------------------------|
| `sorobeacon_monitor`    | Look up a monitor by ID      |

## Building

```sh
cd terraform-provider-sorobeacon
go build -o terraform-provider-sorobeacon
```

The provider has its own `go.mod` to keep its dependencies (terraform-plugin-framework)
out of the server build.

## What is not covered

Templates, saved searches and alerts are not yet resources. Alerts are read-only
by nature; templates and saved searches are low-value for IaC compared to
monitors, rules and channels. Contributions welcome.
