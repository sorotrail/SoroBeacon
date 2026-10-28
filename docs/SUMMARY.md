# Table of contents

* [SoroBeacon](README.md)

## 🚀 Getting started

* [Quickstart](getting-started/quickstart.md)
* [Configuration](getting-started/configuration.md)
* [Docker Compose](getting-started/docker.md)
* [Environment variable reference](configuration.md)

## 🧭 Guides

* [Monitors & alerts](guides/monitors-and-alerts.md)
* [The life of an alert](guides/alert-lifecycle.md)
* [Alert enrichment](guides/alert-enrichment.md)
* [Escalation policies](guides/escalation.md)
* [Maintenance windows](guides/maintenance-windows.md)
* [The dashboard](guides/dashboard.md)
* [Monitoring a token contract](guides/monitoring-a-token.md)
* [Choosing and combining rule types](guides/writing-rules.md)

## 📏 Rule reference

* [Params reference](rules/params.md)
* [event_emitted](rules/event-emitted.md)
* [value_threshold](rules/value-threshold.md)
* [numeric_range](rules/numeric-range.md)
* [token_event](rules/token-event.md)
* [token_supply_change](rules/token-supply-change.md)
* [frequency_threshold](rules/frequency-threshold.md)
* [event_name_glob](rules/event-name-glob.md)
* [contract_allowlist](rules/contract-allowlist.md)
* [Rule cooldown](rules/cooldown.md)
* [self_transfer](rules/self-transfer.md)
* [time_window](rules/time-window.md)
* [absence_of_event](rules/absence-of-event.md)

## 📣 Channel reference

* [Choosing between them](channels/choosing.md)
* [Discord](channels/discord.md)
* [Slack](channels/slack.md)
* [Telegram](channels/telegram.md)
* [ntfy](channels/ntfy.md)
* [Email (SMTP)](channels/email.md)
* [Generic webhook](channels/webhook.md)
* [Federation](channels/federation.md)
* [Matrix](channels/matrix.md)
* [PagerDuty](channels/pagerduty.md)
* [Mattermost](channels/mattermost.md)
* [Rocket.Chat](channels/rocketchat.md)
* [Zulip](channels/zulip.md)
* [Pushover](channels/pushover.md)
* [Message templates](channels/templates.md)
* [External secrets](channels/secrets.md)
* [Digest mode](channels/digest.md)

## 📏 Operations

* [Health and readiness endpoints](operations/health-checks.md)
* [Backing up and restoring the database](operations/backup-restore.md)
* [Securing a SoroBeacon deployment](operations/security.md)
* [Upgrading a running deployment](operations/upgrading.md)
* [Capacity and scaling](operations/scaling.md)
* [Poll priority, reorgs, retention and archiving](operations/retention-and-reorg.md)
* [Alert retention and partitioning](operations/retention.md)
* [Diagnosing failed alert deliveries](operations/delivery-failures.md)
* [Operator dashboard suite](operations/dashboards.md)
* [SLOs and alert library](operations/slos-and-alerts.md)

## 🛠️ Reference

* [Architecture](reference/architecture.md)
* [Authorization](reference/authorization.md)
* [HTTP API](reference/api.md)
* [gRPC API](reference/grpc.md)
* [CLI flags (environment variables)](reference/cli.md)

## 🔌 Contributing

* [A code tour: following one event](contributing/code-tour.md)
* [Extending SoroBeacon](contributing/extending.md)
* [Adding a notification channel](contributing/adding-a-channel.md)
* [Development guide](contributing/development.md)
* [How to run the test suite](contributing/testing.md)