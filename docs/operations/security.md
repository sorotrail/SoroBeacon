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
| `WORKSPACE_TOKENS` | environment | `workspace=token` pairs, each one a bearer credential scoped to a workspace. Logged as the set of workspace names (`workspaces=[acme beta]`), never the tokens. A token that appears under two workspaces, or an id outside `[a-z0-9_-]{1,40}`, fails startup; the error names the entry by position and the offending workspace, and echoes no token material (`internal/config/workspace_tokens.go`). |
| `CONFIG_ENCRYPTION_KEY` | environment | The AES-GCM key for `channels.config`. Validated at startup (`internal/config/config.go`); errors name the allowed key lengths and never echo the value. |
| `OIDC_CLIENT_SECRET` | environment | The identity provider's client secret, sent only to its token endpoint during a login. It is never logged, rendered on a page, or included in an error: the startup line reports `sso_enabled=true` and stops there (`internal/config/config.go`, `LogAttrs`). Nor is any token material — a refused callback logs the reason and the remote address but never the ID token, and an accepted one logs the subject, not the e-mail (`internal/web/oidc.go`). |
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
  A session cookie counts as authentication whether it came from a token or
  from a single sign-on round-trip — either one switches the gate on, and a
  deployment with `OIDC_ISSUER` but no `API_TOKEN` still has no way for a
  script or CI job to reach the API.
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
  SoroBeacon also speaks OIDC itself (`OIDC_ISSUER`,
  [above](#single-sign-on-what-it-delegates-and-what-it-does-not)), which
  covers the dashboard without a second component — but not the probes or
  `/metrics`, so the network boundary above is still the control that matters.

Set `API_TOKEN` regardless of the network posture — it is the control plane's
own credential, and defence in depth here costs one environment variable.

## Workspaces: what isolation does and does not give you

With `WORKSPACE_TOKENS` configured, each credential belongs to one named
workspace, and every monitor, channel, rule, alert, saved search and template
read or written below an authenticated request is filtered to that workspace.
The scope is resolved from the credential (`internal/auth/auth.go`) and carried
in the request context (`internal/api/auth.go`, `internal/web/auth.go`); the
store applies it as a SQL predicate (`internal/store/workspace_scope.go`).

Worth stating precisely, because an isolation feature that is oversold is a
liability:

* **The scope cannot be requested.** There is no `X-Workspace` header and no
  query parameter. A client that could name its own workspace could read
  another team's monitors by changing one header, which is the failure this
  design refuses.
* **It is one process, one database.** Tenancy is a row filter, not a sandbox.
  A workspace's credentials still share the connection pool, the rate limiter
  (per client IP, not per workspace), the poller and every channel webhook URL
  that another workspace cannot see but this process will still call. Database
  access is outside the boundary entirely: anyone readable from the database
  server sees all workspaces.
* **Ingestion and retention are cross-workspace by design.** The poller, the
  retention pruner and reorg handling run outside any request with a system
  scope (`internal/store/workspace_scope.go`), because they must maintain every
  tenant's rows. Alert deduplication is global on `(rule_id, event_id)`, so
  two teams that configure the same rule text against the same contract share
  one fingerprint — they will not each get a duplicate alert, and neither can
  suppress the other's.
* **The guard is a test, not a review comment.** A new store method that
  touches a tenant table without `workspace_id` fails
  `TestEveryScopedQueryCarriesWorkspace` without needing a database, and the
  `WorkspaceIsolation` case inside `TestPostgresConformance` and
  `TestSQLiteConformance` runs the store interface once per workspace against
  both backends. That is what keeps the predicate from being forgotten six
  months from now.

Row-level security was the alternative and was rejected: the SQLite backend has
no RLS at all, so it would protect one deployment and silently not the other,
and under a pooled connection a transaction that forgot `set_config` sees *no*
policy filter — the wrong direction for a bug to fall in.

## Scoped, expiring tokens

`API_TOKEN` and `WORKSPACE_TOKENS` are **static**: they authenticate with full
reach over their workspace, they never expire, and revoking one means editing an
environment variable and restarting the process. That is the right shape for the
operator's own credential and the wrong shape for the twelve CI pipelines that
each need to create one monitor. `/api/v1/tokens` (and the **Tokens** dashboard
page) mints database-backed credentials instead, in `internal/auth/tokens.go`:

* **Scoped.** A token carries a list like `monitors:read monitors:write`. Each
  API route names the scopes it needs (`internal/api/scopes.go`) and the
  middleware checks them, so a token that can create monitors cannot delete a
  channel — and cannot read alert history unless you gave it `alerts:read`.
  `write` on a resource implies `read` on it; `*` carries everything, which is
  `API_TOKEN`'s reach in a form you can revoke.
* **Denied by default.** A route with no entry in the scope table rejects every
  scoped token with `403` rather than allowing it. Forgetting to map a new
  endpoint is a token that cannot reach it, not a token that reaches it freely,
  and `TestScopeTableCoversEveryRoute` fails the build until the mapping is
  written. Static tokens and dashboard sessions are *unrestricted* and pass
  every route, so this cannot lock out an existing deployment.
* **Expiring.** `expires_in` is a duration (`72h`); omit it for a token that
  never expires. An expired token is indistinguishable from a wrong one at
  request time — the same `401`, no clue about why.
* **Revocable without a restart**, and the row survives revocation so the name,
  prefix, scopes and last-use timestamp stay auditable.
* **Least privilege on mint.** A scoped token can only mint tokens whose scopes
  it already holds, so `tokens:write` is not a ladder back to `*`.

### Why the secret is a digest

The `api_tokens` table stores `SHA-256(token)`, never the token
(`internal/store/migrations/0014_api_tokens.up.sql`). A hash is enough because
authentication is an exact-match lookup, and it is the *right* hash function
here rather than bcrypt or argon2 because a minted token is 256 bits from
`crypto/rand` (`sb_` + 43 base64url characters) — there is no offline guess, so
the brute-force resistance a slow KDF buys is worth nothing and would make every
request slow.

Consequences, stated plainly:

* The token is returned **once**, in the `201` body of the mint call. It is not
  listable, not retrievable, and not recoverable by an operator with database
  access — which is the point of the design. Losing it means minting a new one.
* `last_used_at` is displayed alongside a prefix like `sb_3f9a2b71` so a token
  found pasted in a repository can be identified without being stored.
* Nothing but the digest leaks into a log: the creation line records
  `token_id`, `name` and `scopes`.

The dashboard mints inline rather than by redirect: a redirect would make the
secret a URL or a `Referer` header.

## Single sign-on: what it delegates, and what it does not

`OIDC_ISSUER` makes an identity provider decide who may sign in to the dashboard
(`internal/auth/oidc.go`, `internal/web/oidc.go`). It changes *how a browser
proves who it is* and nothing else: the API's authentication, the workspace
boundary and the scope rules all read the same principal as before.

What each piece defends, because a login flow is a chain of exactly this kind of
check and a weakened link is invisible until it is used:

* **Authorization code with PKCE (`S256`), not implicit.** The code is exchanged
  by this process over a back channel, so a token never appears in a URL fragment,
  in browser history, or in a `Referer`. The verifier binds the exchange to the
  login that asked for the code, so a code intercepted in transit is worthless.
* **Signature, issuer, audience and expiry** are checked by
  `github.com/coreos/go-oidc/v3` against the provider's JWKS, with key rotation
  handled by the library. This is why the verification is not hand-rolled: the
  part no one gets right alone is staying correct as the provider's keys change.
* **The nonce** is checked here, not by the library (which leaves it to the
  caller). Without it, an ID token minted for some other relying party at the
  same provider could be presented to this callback and believed.
* **The `state`** is single-use and held server-side, with the nonce, the PKCE
  verifier and the post-login target under it; the browser holds only a copy in
  an `HttpOnly`, `SameSite=Lax` cookie, and the callback must present both. That
  is the login-CSRF defence: an attacker who completes their own provider login
  cannot hand the resulting session to a victim, because the victim's browser
  does not carry the attacker's state cookie.
* **`next`** travels inside the server-side state entry and is filtered by
  `safeNext` when the login starts, so the flow cannot be turned into an
  open redirect to someone else's site.

The parts an operator has to think about, which the code cannot decide:

* **The allow-list is the authorisation.** `OIDC_ALLOWED_DOMAINS` unset means
  every account the provider authenticates gets in — and one account is not a
  user, it is the whole workspace `OIDC_WORKSPACE` names. Leave it unset only
  when the provider's own directory is exactly your operators.
* **`OIDC_WORKSPACE_CLAIM` trusts the claim's contents.** The token is verified,
  so the value is genuinely the provider's, but *any* admitted account can then
  land in *any* workspace its claim names. That is a directory-wide privilege
  decision, and it belongs to whoever administers the provider — which is why
  claim mapping is off unless a variable asks for it.
* **Revocation is asymmetric, and slowly.** Deactivating an account at the
  provider stops the *next* login; it does nothing to a session already issued,
  which runs for its 12 hours (or until the process restarts, which drops every
  session). There is no user table to mark, because there are no users — see
  [the reference](../configuration.md#single-sign-on-oidc). If that window is
  unacceptable, keep `API_TOKEN` as the credential and put SSO in front of it at
  the proxy, where a revoke can drop the connection.
* **A scoped token cannot sign in at `/login`.** A session carries no scope list,
  so logging in with a CI credential would trade a revocable, narrow token for a
  cookie that is neither. SSO sessions are unrestricted within their workspace,
  like a static token's.
* **The client secret is only for the provider.** It never authenticates anything
  at SoroBeacon, and it appears in no log line, page or error; the startup line
  says `sso_enabled=true` and nothing more about the provider.

Register `OIDC_REDIRECT_URL` exactly as the provider expects (default
`/login/oidc/callback`), and remember the session cookie's `Secure` flag follows
whether *this process* saw TLS — behind a terminating proxy, either re-enable TLS
here or accept a cookie that is not marked `Secure`.

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
11. **Every non-operator credential is a scoped token.** Keep `API_TOKEN` as
    the one break-glass credential and give CI jobs, scripts and other
    services a `/api/v1/tokens` mint with the narrowest scope list that works,
    with an expiry. They are then individually revocable and their last use is
    visible, which is what you need when one of them leaks.
12. **If SSO is on, the domain allow-list is set** (`OIDC_ALLOWED_DOMAINS`),
    and the workspace it lands in (`OIDC_WORKSPACE`, or the claim named by
    `OIDC_WORKSPACE_CLAIM`) is the one you meant. Verify: an account outside
    those domains reaches the sign-in page and starts no session — check
    `GET /api/v1/monitors` with its cookie returns `401`.

## See also

* [Environment variable reference](../configuration.md) — every variable,
  defaults, and which ones are secrets.
* [HTTP API](../reference/api.md) — the authentication section, including
  probe exemptions.
* [Backing up and restoring](backup-restore.md) — where the credentials and
  the encryption key live relative to your backups.
