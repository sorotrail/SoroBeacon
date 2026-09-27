# The dashboard

SoroBeacon ships a server-rendered dashboard (Go `html/template` + htmx — no build step, no SPA) at the root of the HTTP listener: [http://localhost:8080](http://localhost:8080).

## Pages

| Page | What you can do |
| --- | --- |
| **Overview** (`/`) | Stats at a glance — monitors, rules, channels, alerts in the last 24h, last ingested ledger — plus a 30-day UTC daily alert chart (inline SVG, no JS charting library) and the most recent alerts. Quiet days are explicit zeroes; an instance with no alerts shows an empty state rather than a flat axis. |
| **Monitors** (`/monitors`) | List, create (name + contract IDs), enable/disable, delete. Click through to a monitor for its rules and channel wiring. |
| **Monitor detail** (`/monitors/{id}`) | Add/delete rules (type + params JSON), attach/detach notification channels with checkboxes. |
| **Channels** (`/channels`) | List, create (type + config JSON), delete — and a **Send test** button that fires a synthetic alert through the real channel and shows the result inline. |
| **Alerts** (`/alerts`) | Paged history with monitor, rule, contract and newest/oldest sort; expand any row to see the full decoded event payload, or **Export CSV** for the whole filtered set. |

{% hint style="info" %}
Channel config is write-only in the UI, same as the API: you paste secrets in when creating a channel, and they are never displayed again.
{% endhint %}

## Signing in

The dashboard has no user accounts. With `API_TOKEN` set it serves a sign-in page at `/login` that accepts any of the configured tokens; on success it sets an HttpOnly, SameSite=Lax session cookie that lasts 12 hours (in memory only — a restart signs everyone out, and re-entering the token is the price of a single shared credential). **Sign out** in the header ends the session.

Permissions are all-or-nothing: anyone holding the token can do everything the dashboard can. There is no roles, no audit trail of who did what, and no per-user limits.

{% hint style="warning" %}
With `API_TOKEN` unset the dashboard is open — the same trust boundary as an unauthenticated API — and logs one startup warning. Set `API_TOKEN` before exposing the port beyond a trusted network. Note that `Secure` is set on the session cookie only when the request arrives over TLS, so terminate TLS at this process (or accept that the cookie travels in clear text on a plain-HTTP deployment).
{% endhint %}

The dashboard is intentionally minimal. A richer UI is an open contributor project — the JSON API under `/api/v1` already exposes everything a SPA would need.
