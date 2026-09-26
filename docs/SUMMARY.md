# Table of contents

* [SoroBeacon](README.md)

## 🚀 Getting started

* [Quickstart](getting-started/quickstart.md)
* [Configuration](getting-started/configuration.md)
* [Docker Compose](getting-started/docker.md)
* [Environment variable reference](configuration.md)

## 🧭 Guides

* [Monitors & alerts](guides/monitors-and-alerts.md)
* [The dashboard](guides/dashboard.md)
* [Monitoring a token contract](guides/monitoring-a-token.md)
* [Choosing and combining rule types](guides/writing-rules.md)

## 📏 Rule reference

* [Params reference](rules/params.md)
* [event_emitted](rules/event-emitted.md)
* [value_threshold](rules/value-threshold.md)
* [token_event](rules/token-event.md)
* [frequency_threshold](rules/frequency-threshold.md)
* [Rule cooldown](rules/cooldown.md)

## 📣 Channel reference

* [Choosing between them](channels/choosing.md)
* [Discord](channels/discord.md)
* [Slack](channels/slack.md)
* [Telegram](channels/telegram.md)
* [Email (SMTP)](channels/email.md)
* [Generic webhook](channels/webhook.md)
* [Matrix](channels/matrix.md)
* [PagerDuty](channels/pagerduty.md)
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

## 🛠️ Reference

* [Architecture](reference/architecture.md)
* [HTTP API](reference/api.md)
* [CLI flags (environment variables)](reference/cli.md)

## 🔌 Contributing

* [A code tour: following one event](contributing/code-tour.md)
* [Extending SoroBeacon](contributing/extending.md)
* [Adding a notification channel](contributing/adding-a-channel.md)
* [Development guide](contributing/development.md)
* [How to run the test suite](contributing/testing.md)