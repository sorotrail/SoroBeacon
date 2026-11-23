# Terraform Provider for SoroBeacon

Manage SoroBeacon monitors, rules and channels as infrastructure-as-code.

**Not published to the Terraform Registry yet.** `terraform init` cannot find
`sorotrail/sorobeacon`, so the install needs a local copy of the binary: a
[filesystem mirror](https://developer.hashicorp.com/terraform/cli/config/config-file#filesystem_mirror)
for shared use, or a
[development override](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides)
while working on the provider. Both are written out step by step, and verified
against a running instance, in
[docs/reference/terraform.md](../docs/reference/terraform.md).

## ⚠️ Channel secrets and Terraform state

Channel config contains credentials: webhook URLs, bot tokens, SMTP passwords.
**Terraform stores attribute values in plain text in state.** The `config`
attribute is marked `Sensitive` so `terraform plan` redacts it in output, but
the state file on disk contains the raw value.

**You must encrypt your state backend.** Use S3 with KMS, GCS with CMEK,
`terraform_remote_state` with encryption, or equivalent. If you use local
state, encrypt the disk. See `docs/channels/secrets.md` for the server-side
half of the same problem.

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

resource "sorobeacon_channel" "slack" {
  name   = "ops-alerts"
  type   = "slack"
  config = jsonencode({ webhook_url = var.slack_webhook_url })
}

resource "sorobeacon_monitor" "payments" {
  name         = "Payment events"
  contract_ids = ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"]
  channel_ids  = [sorobeacon_channel.slack.id]
}

resource "sorobeacon_rule" "transfer" {
  monitor_id = sorobeacon_monitor.payments.id
  type       = "event_emitted"
  # `params` is a JSON string, validated by the server against the rule type.
  params = jsonencode({ event_name = "transfer" })
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

| Resource                        | Description                              |
|---------------------------------|------------------------------------------|
| `sorobeacon_monitor`            | A contract event monitor                 |
| `sorobeacon_rule`               | A rule on a monitor                      |
| `sorobeacon_channel`            | A notification channel                   |
| `sorobeacon_escalation_policy`  | A tiered escalation policy on a monitor  |
| `sorobeacon_maintenance_window` | A scheduled maintenance silence window   |
| `sorobeacon_inhibition`         | An alert inhibition pair between rules   |

## Data Sources

| Data Source             | Description                  |
|-------------------------|------------------------------|
| `sorobeacon_monitor`    | Look up a monitor by ID      |

## Building

```sh
make build-tf              # from the repository root
```

or, from this directory:

```sh
go build -o terraform-provider-sorobeacon
```

The provider has its own `go.mod` to keep its dependencies (terraform-plugin-framework)
out of the server build.

## Documentation

- [`docs/reference/terraform.md`](../docs/reference/terraform.md) — install,
  provider configuration, credential handling, worked example, per-resource
  attribute tables, import and update behaviour.
- [`docs/`](docs/) — the same schema as generated Registry-format reference
  pages, regenerated with:

  ```sh
  go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-name sorobeacon
  ```

## What is not covered

Templates, saved searches and alerts are not yet resources. Alerts are read-only
by nature; templates and saved searches are low-value for IaC compared to
monitors, rules and channels. Contributions welcome.
