# The dashboard

SoroBeacon ships a server-rendered dashboard (Go `html/template` + htmx — no build step, no SPA) at the root of the HTTP listener: [http://localhost:8080](http://localhost:8080).

## Pages

| Page | What you can do |
| --- | --- |
| **Overview** (`/`) | Stats at a glance — monitors, rules, channels, alerts in the last 24h, last ingested ledger — plus a 30-day UTC daily alert chart (inline SVG, no JS charting library) and the most recent alerts. Quiet days are explicit zeroes; an instance with no alerts shows an empty state rather than a flat axis. |
| **Monitors** (`/monitors`) | List, create (name + contract IDs), enable/disable, delete. Click through to a monitor for its rules and channel wiring. |
| **Monitor detail** (`/monitors/{id}`) | Add/delete rules (type + params JSON), attach/detach notification channels with checkboxes. |
| **Channels** (`/channels`) | List, create (type + config JSON), delete — and a **Send test** button that fires a synthetic alert through the real channel and shows the result inline. |
| **Alerts** (`/alerts`) | Paged history with monitor, rule, contract, network and newest/oldest sort; expand any row to see the full decoded event payload, or **Export CSV** for the whole filtered set. |
| **Tokens** (`/tokens`) | Mint, list and revoke scoped API tokens. A new token's secret is shown once, inline on the page, and is never displayed again — the row keeps only its digest. |

{% hint style="info" %}
Channel config is write-only in the UI, same as the API: you paste secrets in when creating a channel, and they are never displayed again. API tokens follow the same rule, for the same reason.
{% endhint %}

{% hint style="info" %}
On an instance polling several networks (`NETWORKS`), the Monitors and Alerts lists gain a chain filter and the Overview shows one row per network — processed ledger, chain tip, lag and last successful poll. A chain that is behind makes the whole Overview read degraded, because "which network is stale" is the question that page has to answer.
{% endhint %}

## Minting a token for a CI job

Signing in with a token and opening **Tokens** gets you a credential you can hand to something else. Name it, tick the scopes it needs, choose an expiry, and copy the secret out of the box that appears — closing the page loses it, and there is no way to read it back.

Scopes are per resource (`monitors:read`, `channels:write`, …), and a credential may only mint tokens carrying scopes it already holds — checked on the server, so `tokens:write` is not a ladder back to full control. Revoke retires a token immediately and leaves the row listed with its last-used timestamp, which is how you find the stale ones. Full details in [HTTP API → Tokens](../reference/api.md#tokens).

Sign-in itself needs a static credential: `API_TOKEN` or a `WORKSPACE_TOKENS` value, or an identity provider if one is configured. A scoped `sb_` token is not a login, because a dashboard session carries no scope list and would silently gain the whole instance.

## Signing in

The dashboard has no user accounts. With `API_TOKEN` set it serves a sign-in page at `/login` that accepts any of the configured tokens; on success it sets an HttpOnly, SameSite=Lax session cookie that lasts 12 hours (in memory only — a restart signs everyone out, and re-entering the token is the price of a single shared credential). **Sign out** in the header ends the session.

Set `OIDC_ISSUER` and the same page grows a **Sign in with `<provider>`** button: an authorization-code login with PKCE against your identity provider, which mints the identical session cookie when the provider's answer checks out. It is additive, so the token form stays. Who may sign in is then the provider's decision, narrowed by `OIDC_ALLOWED_DOMAINS` if you set it; which workspace the session sees is `OIDC_WORKSPACE` (or a claim, with `OIDC_WORKSPACE_CLAIM`) — never anything the browser gets to choose. Details in [Environment variable reference → Single sign-on](../configuration.md#single-sign-on-oidc) and [Single sign-on: what it delegates](../operations/security.md#single-sign-on-what-it-delegates-and-what-it-does-not).

Permissions are all-or-nothing: anyone holding the token — or anyone's provider account that clears the allow-list — can do everything the dashboard can. There is no roles, no audit trail of who did what, and no per-user limits.

{% hint style="warning" %}
With `API_TOKEN` unset the dashboard is open — the same trust boundary as an unauthenticated API — and logs one startup warning. Set `API_TOKEN` before exposing the port beyond a trusted network. Note that `Secure` is set on the session cookie only when the request arrives over TLS, so terminate TLS at this process (or accept that the cookie travels in clear text on a plain-HTTP deployment).
{% endhint %}

The dashboard is intentionally minimal. A richer UI is an open contributor project — the JSON API under `/api/v1` already exposes everything a SPA would need.
