# Terraform provider

SoroBeacon ships a Terraform provider in
[`terraform-provider-sorobeacon/`](https://github.com/sorotrail/SoroBeacon/tree/main/terraform-provider-sorobeacon):
three resources and one data source, covering monitors, rules and channels. The
premise of this project is that monitors, rules and channels are configuration,
so managing that configuration as code is what the provider is for.

The provider is an ordinary client of the [HTTP API](api.md). It holds no
privileged connection, it can do nothing the API would refuse, and the only
state it keeps is Terraform's own. If you can drive `curl` against
`/api/v1`, you can drive the provider.

{% hint style="warning" %}
Channel `config` holds webhook URLs, bot tokens and SMTP passwords, and
**Terraform writes attribute values into state in plain text**. Read
[Channel config is a secret](#channel-config-is-a-secret-in-state) before your
first apply.
{% endhint %}

## Installing it

**The provider is not published to the Terraform Registry.** Asking Terraform
to install it the normal way fails:

```
Error: Failed to query available provider packages

Could not retrieve the list of available versions for provider
sorotrail/sorobeacon: provider registry registry.terraform.io does not have
a provider named registry.terraform.io/sorotrail/sorobeacon
```

That is the real state of the repository, not a misconfiguration, so the
install has to name a local copy of the binary. Two ways, both verified against
a running instance.

### As a local filesystem mirror

This is the one to use for anything shared: a team, a CI job, a module. It goes
through `terraform init` normally and writes a lock file, so the version you
tested against is the version that gets used.

Build the binary and put it where Terraform's [filesystem mirror][fs-mirror]
layout expects — `HOSTNAME/NAMESPACE/TYPE/VERSION/PLATFORM/`, with the
`_vX.Y.Z` filename suffix. `0.1.0` is the version the provider reports in its
handshake, and `linux_amd64` is the platform; both come from
`main.go` and `provider.go` respectively.

```sh
make build-tf     # → bin/terraform-provider-sorobeacon

MIRROR=~/.terraform.d/plugins
install -D -m 0755 bin/terraform-provider-sorobeacon \
  "$MIRROR/registry.terraform.io/sorotrail/sorobeacon/0.1.0/linux_amd64/terraform-provider-sorobeacon_v0.1.0"
```

Then point Terraform at the mirror in its CLI configuration file
(`~/.terraformrc`, or `%APPDATA%\terraform.rc` on Windows). The `include` and
the matching `exclude` are what keep Terraform from trying the registry for
this provider and failing:

```hcl
provider_installation {
  filesystem_mirror {
    path    = "~/.terraform.d/plugins"
    include = ["registry.terraform.io/sorotrail/sorobeacon"]
  }
  direct {
    exclude = ["registry.terraform.io/sorotrail/sorobeacon"]
  }
}
```

From here `terraform init`, `plan` and `apply` behave as they would for a
published provider.

### As a development override

Use this while working on the provider itself. It skips `init` entirely — you
run the binary straight out of a build directory — so every plan picks up your
latest build with no reinstall step.

```hcl
# ~/.terraformrc
provider_installation {
  dev_overrides {
    "sorotrail/sorobeacon" = "/path/to/sorobeacon/bin"
  }
  direct {}
}
```

Note the key is the short `sorotrail/sorobeacon` source, without the
`registry.terraform.io/` prefix, and the value is a *directory*, not a file.
Running `terraform init` with an override in place is not an error — it prints
"Provider development overrides are in effect" and installs nothing.

{% hint style="warning" %}
A development override applies to every Terraform command, including `init` and
`destroy`, on that machine. Keep it in a scratch CLI config
(`TF_CLI_CONFIG_FILE=dev.tfrc terraform plan`) rather than your real one, or
your production runs will execute whatever binary happens to be in that
directory.
{% endhint %}

## Provider configuration

```hcl
terraform {
  required_providers {
    sorobeacon = {
      source = "sorotrail/sorobeacon"
    }
  }
}

provider "sorobeacon" {
  endpoint = var.sorobeacon_endpoint   # e.g. https://beacon.example.com/api/v1
  token    = var.sorobeacon_token      # omit only on an unauthenticated instance
}
```

| Attribute | Type | Required | Meaning |
| --- | --- | --- | --- |
| `endpoint` | string | yes | API base URL, including the `/api/v1` path segment. Every request the provider makes is this URL plus a resource path, so a value without the segment produces a 404 on the first create. |
| `token` | string | no, sensitive | Bearer token sent as `Authorization: Bearer <token>`. Only optional because an instance with `API_TOKEN` unset accepts unauthenticated requests. Marked sensitive, so `terraform plan` redacts it. |

### Which token it needs

The provider authenticates as a normal API client, so a
[scoped token](api.md#tokens) is the right credential. For a full lifecycle —
create, update and destroy — mint one with:

```sh
curl -s -X POST https://beacon.example.com/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"name":"terraform","scopes":["monitors:read","monitors:write","channels:read","channels:write"]}'
```

Those four are derived from the routes the provider actually calls, read
against the [scope table](api.md#tokens):

* `monitors:read` — `GET /monitors/{id}` and `GET /monitors/{id}/rules`, which
  every refresh and every `terraform plan` issues.
* `monitors:write` — monitor and rule create, update and delete.
* `channels:read` — `GET /channels/{id}` during refresh.
* `channels:write` — channel create, update and delete.

A plan-only role can drop both `write` scopes. There is no separate scope for
the provider: the checks are per route, so a token that can create a monitor
through the dashboard can also create one through Terraform, which is the
point. A static `API_TOKEN` works everywhere a scoped token does, and is
unscoped in both directions.

## Channel config is a secret in state

Stated plainly, because it is the one thing that can hurt:

* **`sorobeacon_channel.config` is written into your Terraform state file as a
  JSON string, in plain text, containing whatever you put there** — the Slack
  webhook URL, the Telegram bot token, the SMTP password. Verified: after an
  apply, `grep` the state file for the webhook URL and it is there.
* The attribute is marked `Sensitive`, so `terraform plan` shows
  `(sensitive value)` instead of the JSON. That is a *display* redaction. It
  does not encrypt anything, and it does not stop the value being written.
* The API never returns a channel's config, consistent with the rest of the
  surface ([see the HTTP API](api.md#channels)). The provider therefore keeps
  whatever value you last applied rather than reading it back — the value in
  state is the value you wrote, not a copy of what the server holds.
* So: **treat the state file as a secret.** Use an encrypted backend — S3 with
  KMS, GCS with CMEK, or `terraform_remote_state` with encryption — and keep it
  out of version control. Local state on an encrypted disk is the minimum,
  not the goal.
* Server-side, `CONFIG_ENCRYPTION_KEY` encrypts channel config at rest in the
  database. That is a different protection: it does nothing for the copy
  Terraform makes on your machine. Both are needed; neither substitutes for the
  other.

For what each channel's config must contain, and how to keep secrets out of it
in the first place, see [External secrets](../channels/secrets.md).

## A worked example

A channel, a monitor and a rule, and how they connect. This is the
configuration the rest of this page was checked against, applied against a
running instance.

```hcl
provider "sorobeacon" {
  endpoint = "http://127.0.0.1:8099/api/v1"
  token    = var.sorobeacon_token
}

# 1. Somewhere for alerts to go. `type` selects the notifier; `config` is
#    that notifier's own JSON, so its shape depends on the type.
resource "sorobeacon_channel" "ops" {
  name = "ops-alerts"
  type = "slack"
  config = jsonencode({
    webhook_url = var.slack_webhook_url
  })
}

# 2. What to watch. The monitor owns the contract list and points at the
#    channels to alert, which is why it takes the channel's numeric id.
resource "sorobeacon_monitor" "payments" {
  name         = "Payment events"
  contract_ids = ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"]
  channel_ids  = [sorobeacon_channel.ops.id]
}

# 3. What counts as noteworthy. A rule always belongs to a monitor, so it
#    takes the monitor's id; `params` is the rule type's own JSON, validated
#    by the server at apply time.
resource "sorobeacon_rule" "transfer" {
  monitor_id = sorobeacon_monitor.payments.id
  type       = "event_emitted"
  params = jsonencode({
    event_name = "transfer"
  })
}

output "monitor_id" {
  value = sorobeacon_monitor.payments.id
}
```

The chain is entirely in Terraform's references: the channel is created first
because the monitor's `channel_ids` interpolates its id, and the rule last
because it needs the monitor's. Nothing has to be looked up by hand, and
nothing is created in the wrong order when a dependency is added later.

```sh
terraform apply
# sorobeacon_channel.ops: Creation complete after 0s [name=ops-alerts]
# sorobeacon_monitor.payments: Creation complete after 0s [name=Payment events]
# sorobeacon_rule.transfer: Creation complete after 0s
```

An invalid `contract_ids` entry or a rule `params` the server rejects fails at
apply time with the API's own validation message, naming the field —
`invalid contract id: …`, `event_emitted: set event_name and/or topic_equals`.
Nothing is written, so a failed apply leaves the successfully created resources
alone rather than half-configured.

The rule types and their parameters are documented per type; start with the
[params reference](../rules/params.md).

## Resources and data source

### `sorobeacon_monitor`

A monitor: a set of contract addresses polled for events, and the channels
alerted when a rule fires.

| Attribute | Type | Mode | Meaning |
| --- | --- | --- | --- |
| `id` | number | computed | Numeric monitor ID. |
| `name` | string | required | Display name. |
| `contract_ids` | list(string) | required | Soroban contract addresses to watch, validated as strkeys by the server. |
| `enabled` | bool | optional, defaults to `true` | Whether the monitor is polled. |
| `channel_ids` | list(number) | optional | Channels to alert. Replaces the whole list on update, matching the API. |

### `sorobeacon_rule`

A rule on a monitor.

| Attribute | Type | Mode | Meaning |
| --- | --- | --- | --- |
| `id` | number | computed | Numeric rule ID. |
| `monitor_id` | number | required | The parent monitor. Rules are always created under a monitor. |
| `type` | string | required | Rule type, e.g. `event_emitted`, `value_threshold`. Validated by the server. |
| `params` | string | required | The rule's parameters as a **JSON string**. Use `jsonencode()`; the value is validated per type on create and update. |
| `enabled` | bool | optional, defaults to `true` | Whether the rule is evaluated. |

### `sorobeacon_channel`

A notification channel. Read the warning at the top of this page before
writing one.

| Attribute | Type | Mode | Meaning |
| --- | --- | --- | --- |
| `id` | number | computed | Numeric channel ID. |
| `name` | string | required | Display name. |
| `type` | string | required | Notifier type. The server accepts `discord`, `slack`, `telegram`, `email`, `webhook`, `ntfy`, `matrix`, `pagerduty`, `federation`, `twilio`, `signal`, `webex`, `lark`, `dingtalk`, `mattermost`, `rocketchat`, `zulip`, `pushover`, `googlechat`, `opsgenie`, `gotify`, `sns` and `jsontemplate`; see the [channel reference](../channels/choosing.md) for what each one needs. |
| `config` | string | required, sensitive | The notifier's configuration as a JSON string. Contains secrets, and is written to state in plain text. |
| `enabled` | bool | optional, defaults to `true` | Whether the channel is in rotation. Note that the server's channel *health* fields — `consecutive_failures`, `disabled_at` and the rest — are not managed here, so a channel the server auto-disabled will be re-enabled by any apply that touches it. |

### `sorobeacon_monitor` (data source)

Reads an existing monitor by ID. Use it to let another module consume a monitor
this configuration created, rather than passing the id around by hand.

```hcl
data "sorobeacon_monitor" "payments" {
  id = sorobeacon_monitor.payments.id
}
```

| Attribute | Type | Mode | Meaning |
| --- | --- | --- | --- |
| `id` | number | required | The monitor to look up. |
| `name` | string | computed | |
| `contract_ids` | list(string) | computed | |
| `enabled` | bool | computed | |
| `channel_ids` | list(number) | computed | |

A monitor that does not exist fails the read rather than returning an empty
data source, so a typo is an error at plan time.

## Updates and replacement

**Nothing forces replacement.** The schemas carry no `RequiresReplace` plan
modifiers, so every attribute on every resource is an in-place update: a
changed `name`, `enabled`, `contract_ids`, `channel_ids`, `type` or `params`
produces `~ update in-place`, never `-/+`. Terraform shows this as an update in
its plan output, and the resource's numeric `id` — and so every alert, rule and
delivery already pointing at it — survives the change.

That is worth knowing in both directions. There is no replacement to plan
around, and equally nothing stops a change you did not intend: a `t channel`
switch to a type whose config you did not write rewrites the channel's
notifier in place.

Two behaviours around out-of-band deletion, both verified:

* Deleting a **monitor or channel** outside Terraform does not plan a
  re-create. The refresh calls `GET`, gets a 404, and the plan fails outright
  with `Read monitor failed: not found` until you remove the resource from
  configuration by hand. This is a provider bug, not a design choice, and it is
  reported separately.
* Deleting a **rule** outside Terraform plans a re-create, because the rule's
  refresh lists the monitor's rules and treats a missing id as absent.

## Import

All three resources import, so adopting an existing instance is a first-class
operation rather than a migration script:

```sh
terraform import sorobeacon_channel.ops 1
terraform import sorobeacon_monitor.payments 1
terraform import sorobeacon_rule.transfer 1/1    # monitor_id/rule_id
```

Import needs the resource to already exist in configuration — Terraform will
not import into an address that has no block — so write the block first with
placeholder values, import, then replace the placeholders with the real ones.

**Importing a channel cannot recover its `config`**, because the API never
returns it. State comes back with `config = null`, and the next plan shows your
configured value as a change that an apply then writes to the channel. That is
the intended way to adopt one: put the real config in your configuration
before applying, and expect the first apply to overwrite whatever the channel
was configured with. If you do not know the current config, read it out of band
first — the provider cannot.

## What the provider does not manage

Deliberately, not by omission:

* **Alerts.** They are read-only by nature — they are produced by rules, not
  declared by configuration. Manage the rule that raises them instead.
* **Templates and saved searches.** Low-value for infrastructure-as-code
  compared to the objects above.
* **Per-channel delivery settings** — `digest_mode`, `digest_window_seconds`
  and `min_severity`. The API accepts them; the channel resource does not
  expose them. A channel managed here is a channel whose digest settings are
  whatever the server has, and an apply that rewrites `config` does not disturb
  them, but Terraform will not report a drift in them.

## Generated reference pages

`terraform-provider-sorobeacon/docs/` (in the repository, not in this book)
carries the same schema as a [Registry-format][registry-layout] reference —
`index.md`, `resources/` and `data-sources/` — generated from the provider
schemas by [`tfplugindocs`][tfplugindocs] rather than written by hand, so it
cannot drift from the code. It is what publishing to the Registry would
require, and it is regenerated with:

```sh
cd terraform-provider-sorobeacon
go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-name sorobeacon
```

This page is the one to read: it carries the install path, the credential
handling and the behaviour notes that a generated schema listing cannot.

[fs-mirror]: https://developer.hashicorp.com/terraform/cli/config/config-file#filesystem_mirror
[registry-layout]: https://developer.hashicorp.com/terraform/registry/providers/docs
[tfplugindocs]: https://github.com/hashicorp/terraform-plugin-docs