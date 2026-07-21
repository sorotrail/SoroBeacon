# The dashboard

SoroBeacon ships a server-rendered dashboard (Go `html/template` + htmx — no build step, no SPA) at the root of the HTTP listener: [http://localhost:8080](http://localhost:8080).

## Pages

| Page | What you can do |
| --- | --- |
| **Overview** (`/`) | Stats at a glance — monitors, rules, channels, alerts in the last 24h, last ingested ledger — plus the most recent alerts. |
| **Monitors** (`/monitors`) | List, create (name + contract IDs), enable/disable, delete. Click through to a monitor for its rules and channel wiring. |
| **Monitor detail** (`/monitors/{id}`) | Add/delete rules (type + params JSON), attach/detach notification channels with checkboxes. |
| **Channels** (`/channels`) | List, create (type + config JSON), delete — and a **Send test** button that fires a synthetic alert through the real channel and shows the result inline. |
| **Alerts** (`/alerts`) | Paged history with a per-monitor filter; expand any row to see the full decoded event payload. |

{% hint style="info" %}
Channel config is write-only in the UI, same as the API: you paste secrets in when creating a channel, and they are never displayed again.
{% endhint %}

{% hint style="warning" %}
The dashboard has no authentication in the MVP — it is the same trust boundary as the API. Don't expose it to the public internet unprotected.
{% endhint %}

The dashboard is intentionally minimal. A richer UI is an open contributor project — the JSON API under `/api/v1` already exposes everything a SPA would need.
