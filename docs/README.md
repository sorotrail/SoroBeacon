# SoroBeacon

**The monitoring beacon for Soroban smart contracts — point it at any contract on Stellar, define rules, and get alerted the moment something happens on-chain.**

Stellar has no good open-source way to watch a contract and get notified when an event fires or a value crosses a threshold. SoroBeacon fills that gap as a self-hostable public good for the Soroban ecosystem: a small Go service that polls a Stellar RPC node, runs your rules against every decoded contract event, and delivers alerts wherever your team lives.

{% hint style="warning" %}
Soroban RPC nodes only retain events for roughly **24 hours to 7 days**. Historical backfill is impossible — a monitor only catches what happens while SoroBeacon is running. Keep it deployed continuously.
{% endhint %}

## What it does

| Piece | What you get |
| --- | --- |
| **Monitors** | Watch one or more contract addresses per monitor |
| **Rules** | [`event_emitted`](rules/event-emitted.md) and [`value_threshold`](rules/value-threshold.md), evaluated against every event |
| **Channels** | [Discord](channels/discord.md), [Slack](channels/slack.md), [Telegram](channels/telegram.md), [email](channels/email.md), [HMAC-signed webhooks](channels/webhook.md) |
| **Delivery** | Retries with exponential backoff, every attempt recorded |
| **Dedup** | An alert fires at most once per `(rule, event)` — restarts never double-notify |
| **Dashboard** | Server-rendered UI to manage monitors, channels, and alert history |

{% hint style="info" %}
SoroBeacon is deliberately a **minimal, well-tested core with clean extension points**. New rule types and channels are single-interface implementations — see [Extending SoroBeacon](contributing/extending.md).
{% endhint %}

## Jump in

* [Quickstart](getting-started/quickstart.md) — running against testnet in one `docker compose up`
* [Monitors & alerts](guides/monitors-and-alerts.md) — wire a contract to a Discord channel end to end
* [HTTP API](reference/api.md) — every endpoint with `curl` examples
* [Extending SoroBeacon](contributing/extending.md) — add your own rule type or channel
