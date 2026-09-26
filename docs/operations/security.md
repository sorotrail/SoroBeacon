# Securing a SoroBeacon deployment

SoroBeacon holds two kinds of valuable state: **channel credentials** (webhook
URLs, bot tokens, SMTP passwords in `channels.config`) and **control of the
instance itself** (anyone who can reach the API can create, rewrite and delete
monitors and channels, and therefore choose where alerts are delivered). This
page tells an operator what the software already does to protect both, what it
deliberately does **not** do, and what to put in front of it before going
live. Every claim here is traceable to the file named inline; if this page and
the code disagree, the code wins and this page should be updated.

## What is secret, and how it is handled

### Channel config

A channel's `config` JSON holds the credentials for that destination — a
Discord or Slack webhook URL, a Telegram bot token, an SMTP username and
password, a webhook signing secret. Three properties hold, all rooted in the
type definitions in `internal/store/store.go`:

* **Never returned by the API.** `Channel.Config` is marshalled with
  `json:"-"`, so no API response or dashboard page that encodes a
  `store.Channel` includes it. `POST /channels` and `PATCH /channels/{id}`
  accept a config; nothing ever reads one back.
* **Never logged.** The codebase's rule — stated in `CONTRIBUTING.md` and
  enforced by hand in every write path — is that channel config contents stay
  out of log lines, error messages and delivery records. When a channel
  config cannot be decrypted, the error names the channel by name or id and
  never echoes ciphertext or key material (`internal/store/crypto.go`).
* **Encrypted at rest when `CONFIG_ENCRYPTION_KEY` is set.** With a key
  configured, the store writes an AES-GCM envelope instead of plaintext
  (`internal/store/crypto.go`); rows written before a key existed keep
  working as plaintext and are re-encrypted on their next write. Without a
  key the column is plaintext and the process logs one warning at startup
  (`cmd/sorobeacon/main.go`). Losing the key makes encrypted configs
  unrecoverable — back it up with the database
  ([backing up](backup-restore.md)).

Two places where a leak would be easy are covered by construction:

* **Delivery attempts.** The dispatcher records failures as a
  `response_snippet` and logs the error (`internal/notify/dispatcher.go`).
  The notifiers are written so those errors never contain the destination:
  `internal/notify/http.go` strips the request URL from `net/http` transport
  errors (`redactURLError`) before wrapping, because webhook URLs and bot
  tokens live in paths and query strings. A failed Telegram delivery says
  `post: dial tcp: ...` without the `https://api.telegram.org/bot<token>`
  URL.
* **Access logs.** The request logger emits the matched chi route pattern
  (e.g. `/api/v1/channels/{id}/test`), not the raw path or query
  (`internal/api/requestlog.go`), so a token pasted into a query string never
  reaches the log. Query strings, headers and bodies are never logged.

### The other credentials

| Secret | Where it lives | Notes |
| --- | --- | --- |
| `DATABASE_URL` | environment | Often embeds a password. See [log redaction](#database_url-in-logs) for what appears in logs. |
| `API_TOKEN` | environment | Bearer credential for the API and dashboard sign-in. Never logged; only the count of configured tokens appears at startup (`internal/config/config.go`, `LogAttrs`). |
| `CONFIG_ENCRYPTION_KEY` | environment | The AES-GCM key for `channels.config`. Validated at startup (`internal/config/config.go`); errors name the allowed key lengths and never echo the value. |
| `ARCHIVE_URL` | environment | May embed an endpoint or token in its query string. Only the fact that archiving is enabled is logged (`cmd/sorobeacon/main.go`). |

## The API has no authentication of its own

State this plainly: **with `API_TOKEN` unset, SoroBeacon's HTTP listener is
unauthenticated.** Anyone who can reach the port can create, rewrite and
delete monitors and channels, read alert history, and trigger test deliveries
to wherever existing channels point. The process logs one startup warning and
carries on (`cmd/sorobeacon/main.go`) — the behaviour is deliberate, so the
docker-compose quickstart and upgrades never lock an operator out, but it is
not a posture to take public.

Three facts about the built-in authentication, all implemented in
`internal/api/auth.go` and `internal/auth/auth.go`:

* When `API_TOKEN` is set, every `/api/v1` route requires
  `Authorization: Bearer <token>` (or a dashboard session cookie minted from
  the same token at `/login`). A missing header, a malformed one and a wrong
  token return the same `401`; the presented credential is never echoed.
* **The probes are exempt**: `/api/v1/health`, `/livez` and `/readyz`
  answer without a token, and `/readyz` reports per-dependency detail —
  including dependency error strings — to anyone who can reach the port.
* **`/metrics` on the same listener is not behind the API authentication at
  all** — it is mounted on the root router (`cmd/sorobeacon/main.go`) before
  any auth middleware applies.

Because the exemptions are load-bearing (orchestrator health checks, the
compose healthcheck), the boundary has to be network-level:

### What to put in front of it

Do **not** publish the listener directly to the internet. Put one of these in
front, and keep the port itself on a private network or loopback:

* **A reverse proxy on a private network** (nginx, Caddy, Traefik) that
  terminates TLS, applies your own IP allow-list or SSO, and forwards to
  `127.0.0.1:8080`. This is the minimum for anything internet-reachable.
* **A VPN / private network** (WireGuard, Tailscale, a VPC security group):
  the listener stays bound to a private interface, and only your operators'
  networks can reach it at all.
* **An authenticating proxy** (oauth2-proxy, Pomerium) in front of the whole
  listener if you want browser SSO in addition to `API_TOKEN`; note the
  probes will then also require the proxy's session, so point health checks
  at the proxy's own unauthenticated endpoint or exempt those paths there.

Set `API_TOKEN` regardless of the network posture — it is the control plane's
own credential, and defence in depth here costs one environment variable.

## What the software already does: request limits

Two protections are built in, both off-or-defaulted so existing deployments
are unaffected until an operator opts in:

* **Request body limit.** Write endpoints (`POST`/`PATCH`/`PUT`) wrap the
  body in `http.MaxBytesReader` and reject larger requests with `413` before
  reading them; `GET`/`HEAD`/`OPTIONS` are untouched
  (`internal/api/maxbody.go`). The default is 1 MiB
  (`internal/api/api.go`, `DefaultMaxBodyBytes`), controlled by
  `HTTP_MAX_BODY_BYTES` (parsed in `internal/config/config.go`; zero or
  negative values are rejected and a non-positive wiring cannot disable the
  cap).
* **Per-client rate limiting.** A token-bucket limiter keyed by client
  address, opt-in via `RATE_LIMIT_RPS` (burst defaults to
  ⌈RPS⌉, `RATE_LIMIT_BURST` overrides). Excess requests get `429` with
  `RateLimit-*` and `Retry-After` headers; `/health`, `/livez` and `/readyz`
  stay exempt so a probe loop cannot rate-limit itself into a restart
  (`internal/api/ratelimit.go`). By default the client key is `RemoteAddr`;
  enable `RATE_LIMIT_TRUST_FORWARDED` **only** behind a proxy that overwrites
  `X-Forwarded-For`, otherwise a spoofed header defeats the limit.

Both middlewares run on the API router; the rate limiter sits after the auth
middleware (`internal/api/api.go`), so unauthenticated floods are rejected at
the `401` without allocating limiter buckets. Neither middleware applies to
`/metrics`, and rate limiting does not apply to the dashboard — a reverse
proxy with its own limits is still the right front door.

## `DATABASE_URL` in logs

At startup the effective configuration is logged once as structured
attributes (`internal/config/config.go`, `Config.LogAttrs` — the one place
configuration is printed, and fields are opted in, so a new config field is
not logged until someone lists it).

`database_url` in that line is **redacted to scheme, host (with port) and
database name**: userinfo (the password), query string and fragment are
dropped (`redactDatabaseURL` in `internal/config/config.go`). A line reads
`postgres://dbhost:5432/sorobeacon` — no `user:pass@`.

What this does **not** cover:

* Log lines emitted by the **database driver or Postgres itself** are outside
  SoroBeacon's redaction. A connection failure surfaced by pgx can include
  the DSN; send driver-facing logs somewhere that treats `DATABASE_URL` as a
  secret.
* The **SQLite** path keeps the file path in the startup line by design —
  the path *is* the database and carries no credentials
  (`internal/config/config.go`).
* Anything an operator does downstream of the process (a shell exporting the
  variable, a supervisor's environment dump, a compose file committed to
  git) is not redactable by software. Keep `DATABASE_URL`, `API_TOKEN` and
  `CONFIG_ENCRYPTION_KEY` out of shell history and version control.

## Pre-launch checklist

Run through this before pointing the listener at anything beyond localhost:

1. **`API_TOKEN` is set** to a generated value (`openssl rand -hex 32`) and
   clients send `Authorization: Bearer`. Verify: an unauthenticated request
   to `/api/v1/monitors` returns `401`.
2. **The listener is not internet-reachable**: bound to a private interface
   or loopback (`HTTP_ADDR`), with a proxy/VPN in front as
   [above](#what-to-put-in-front-of-it), and no port-forward from the
   internet directly to `:8080`.
3. **`/metrics` and the probes are treated as public** at the front door:
   either exempt them deliberately (they reveal health detail, not secrets)
   or restrict them at the proxy. Do not assume the API token covers them.
4. **`CONFIG_ENCRYPTION_KEY` is set** (`openssl rand -base64 32`) so channel
   credentials are encrypted at rest, and the key is backed up with the
   database. Verify: no startup warning about channel config encryption.
5. **The startup log line shows `database_url` without a password** —
   `postgres://host:port/dbname`, no userinfo.
6. **`DATABASE_URL`, `API_TOKEN` and `CONFIG_ENCRYPTION_KEY` live in a
   secret manager or a `0600`-mode `.env`** that is not committed, not in a
   compose file, and not in shell history.
7. **TLS terminates in front** of the listener (proxy) so tokens and
   dashboard sessions are not sent in cleartext over any network you do not
   control.
8. **Rate limiting is on** if the instance is reachable by anything
   untrusted: `RATE_LIMIT_RPS` (and `RATE_LIMIT_BURST` if you want a bucket
   other than ⌈RPS⌉), with `RATE_LIMIT_TRUST_FORWARDED=true` only behind a
   proxy that overwrites `X-Forwarded-For`.
9. **Channel test-sends and alert history contain no credentials** — spot
   check a `delivery_attempts.response_snippet` after forcing one failure;
   it should describe the failure without the URL or token.
10. **CORS stays off** (`CORS_ALLOWED_ORIGINS` unset) unless a browser app
    you control needs it; an allowed origin can act with whatever credential
    its users hold.

## See also

* [Environment variable reference](../configuration.md) — every variable,
  defaults, and which ones are secrets.
* [HTTP API](../reference/api.md) — the authentication section, including
  probe exemptions.
* [Backing up and restoring](backup-restore.md) — where the credentials and
  the encryption key live relative to your backups.
