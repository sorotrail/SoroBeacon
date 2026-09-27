# Digest mode

Some monitors are informational: you want to know what a contract did, but
not one message per event. Deduplication collapses *identical* alerts;
digesting batches *different* ones over a window and sends a single summary.

Digest is a per-channel setting and is **off by default**, so existing
channels deliver exactly as before.

## Enabling it

Digest settings are part of a channel. Set them through the API:

```json
{
  "name": "ops digest",
  "type": "slack",
  "config": { "webhook_url": "..." },
  "digest_mode": "window",
  "digest_window_seconds": 900
}
```

| Field                   | Meaning                                                                 |
|-------------------------|-------------------------------------------------------------------------|
| `digest_mode`           | `""` (default) delivers immediately; `"window"` batches.                |
| `digest_window_seconds` | How long alerts accumulate before one summary is sent. Must be > 0 when the mode is `window`. |

## Behaviour

- Alerts accumulate in the database, not memory, so a restart does not drop
  a partial window.
- When the window elapses, the dispatcher sends **one** message and clears
  the window. The dispatcher checks every 30 seconds, so a window closes
  within half a minute of its expiry.
- An empty window sends nothing at all.
- The summary states how many alerts it covers and groups them by monitor:

  ```
  SoroBeacon digest: 4 alert(s) across 2 monitor(s)

  payments (3)
  - transfer  C...ABC  ledger 1201  2026-09-26 12:00:00 UTC
  ...

  treasury (1)
  - ...
  ```

- A long digest is truncated to `DefaultDigestMaxLen` (2000 bytes) and ends
  with an explicit `... and N more` line rather than being cut off
  mid-line.
- Digesting affects **delivery only**. Every alert stays individually
  visible in the dashboard and `GET /alerts`; delivery attempts record the
  digest send.
- If a digest send fails, the window is kept and retried on the next tick.

## Per-alert templates

A channel's custom message template (`template` in the config) applies to
individual alerts. A digest summary is already rendered and is sent
verbatim, so it is not reshaped by the per-alert template.
