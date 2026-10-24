# Authorization reference

SoroBeacon has two authorization layers, and they answer different questions:

* **Authentication** — is this request carrying a credential the process
  accepts? (`internal/auth/auth.go`, `internal/api/auth.go`)
* **Authorization** — what may that credential reach? There are two answers to
  that, and a request has to satisfy both: a **role** (`viewer`, `editor`,
  `admin`) and, for database-backed tokens, a **scope list** checked against
  a route table (`internal/auth/tokens.go`, `internal/api/scopes.go`).

Roles are enforced on both surfaces: the API router and the dashboard router
each mount the same middleware over their own route table
(`internal/api/roles.go`, `internal/web/roles.go`). See [Roles](#roles) before
relying on them.

This page is the complete enumeration. If the code and this page disagree, the
code wins; the file to read is named beside each claim.

## Credentials

A request may present exactly one credential. The first one that resolves wins,
and a credential that is present but invalid does **not** fall through to an
anonymous request.

| Credential | Where it comes from | Scope of access | Workspace |
| --- | --- | --- | --- |
| Static bearer token | An entry in `API_TOKEN` | Unrestricted within its workspace | The default workspace (or the one named in `WORKSPACE_TOKENS`) |
| Workspace bearer token | An entry in `WORKSPACE_TOKENS` (`workspace=token`) | Unrestricted within its workspace | The named workspace |
| Scoped bearer token | Minted by the token manager; recognisable by the `sb_` prefix | Exactly the scopes in its database row | The row's workspace |
| Dashboard session cookie | Set by `/login` after a static or workspace token, or an OIDC round-trip | Unrestricted within its workspace | Inherited from the credential that signed in |
| OIDC identity | Verified ID token at the callback | Unrestricted within the mapped workspace | `OIDC_WORKSPACE`, or the workspace named by a configured claim |

Bearer tokens are read from `Authorization: Bearer <token>` (scheme
case-insensitive). Sessions are read from the `sorobeacon_session` cookie.
There is deliberately **no** `X-Workspace` header and no workspace query
parameter: a caller that could name its own tenant could read another's
(`internal/auth/auth.go`).

### Status codes

When authentication is enabled (`API_TOKEN`, `WORKSPACE_TOKENS` or
`OIDC_ISSUER` configured), every `/api/v1` route requires a credential except
`/health`, `/livez` and `/readyz`:

* **`401`** — no credential, a malformed `Authorization` header, an unknown or
  expired token, or an invalid session. The response carries
  `WWW-Authenticate: Bearer realm="sorobeacon"`. An expired scoped token is
  indistinguishable from a wrong one.
* **`403`** — the credential is valid but not allowed. Three messages occur:
  `"token lacks scope <scope>"` (the token is scoped and lacks a required
  grant), `"this route accepts no scoped token"` (the route has no entry in
  the scope table) and `"forbidden"` (the credential's role does not reach
  the route). None of the messages is a secret; naming the missing scope or
  the required role is what lets a caller ask for the right one.
* **`503`** — a valid request to a route whose backing feature is not wired
  into this process, such as token management (see
  [Known gaps](#known-gaps-worth-knowing)).

Static tokens and dashboard sessions are **unrestricted**: they pass every
scope check. Only `sb_` tokens carry a scope list.

## Scopes

A scoped token holds a set drawn from this vocabulary
(`internal/auth/tokens.go`):

| Scope | Grants |
| --- | --- |
| `monitors:read` / `monitors:write` | Read / create, update and delete monitors, rules, imports, duplicates |
| `channels:read` / `channels:write` | Read / create, update, delete and test channels |
| `alerts:read` / `alerts:write` | Read alert history and deliveries / acknowledge, retry, ingest, and manage inhibitions, maintenance windows and dead letters |
| `templates:read` / `templates:write` | Read / create, update, delete and instantiate templates |
| `stats:read` | Read instance and per-monitor statistics |
| `tokens:read` / `tokens:write` | List / mint and revoke API tokens |
| `*` | Every scope |
| `none` | No scope; used by routes that need a valid credential and nothing more |

Rules that hold across the table:

* **`write` implies `read` on the same resource.** `monitors:write` lets a
  token read monitors too; the reverse is not true.
* **A route's scopes are AND-ed.** Creating a monitor from a template requires
  `templates:write` **and** `monitors:write`; a token that can only edit
  templates cannot use one to add monitors. Editing an escalation policy
  requires `monitors:write` **and** `channels:write`, because the policy
  decides which channels the monitor reaches.
* **The table is deny-by-default.** A route with no entry rejects every scoped
  token with `403`; forgetting to map a new endpoint cannot silently grant
  access. `TestScopeTableCoversEveryRoute` walks the router and fails if the
  table and the routes drift in either direction.
* **Least privilege on mint.** A scoped token can only mint tokens whose scopes
  it already holds, so `tokens:write` is not a path back to `*`. An unknown or
  misspelled scope is a `400` naming the entry.
* **`*` is static-token reach in revocable form.** Grant it only deliberately.

### Route-by-route table

These are the patterns under `/api/v1` (`internal/api/scopes.go`). Scopes are
listed in addition to a valid credential; static tokens and sessions bypass
them.

| Routes | Required scopes |
| --- | --- |
| `GET /monitors`, `GET /monitors/{id}`, `GET /monitors/{id}/rules`, `GET /monitors/trash`, `POST /monitors/{id}/rules/dry-run` | `monitors:read` (dry-run also `alerts:read`) |
| `POST /monitors`, `POST /monitors/bulk`, `PATCH`/`DELETE /monitors/{id}`, `POST /monitors/{id}/duplicate`, `POST /monitors/{id}/soft-delete`, `POST /monitors/{id}/restore`, `POST /monitors/{id}/rules`, `POST /monitors/{id}/rules/bulk`, `PATCH`/`DELETE /monitors/{id}/rules/{ruleID}`, `POST /monitors/import`, `DELETE /monitors/{id}/escalation` | `monitors:write` |
| `GET /monitors/{id}/escalation` | `monitors:read` |
| `PUT /monitors/{id}/escalation` | `monitors:write` + `channels:write` |
| `GET /channels`, `GET /channels/{id}` | `channels:read` |
| `POST /channels`, `POST /channels/bulk`, `POST /channels/{id}/duplicate`, `PATCH`/`DELETE /channels/{id}`, `POST /channels/{id}/test` | `channels:write` |
| `GET /channels/{id}/stats` | `stats:read` |
| `GET /templates`, `GET /templates/{id}` | `templates:read` |
| `POST /templates`, `PATCH`/`DELETE /templates/{id}`, `POST /templates/{id}/instantiate`, `POST /templates/{id}/instantiate/bulk` | `templates:write` (instantiate also `monitors:write`) |
| `GET /alerts`, `GET /alerts.csv`, `GET /alerts/export`, `GET /alerts/stream`, `GET /alerts/{id}/deliveries`, `GET /dead-letters`, `GET /inhibitions`, `GET /maintenance-windows`, `GET /maintenance-windows/{id}` | `alerts:read` |
| `POST /alerts/{id}/acknowledge`, `POST /alerts/{id}/deliveries/{channelID}/retry`, `POST /ingest`, `POST /dead-letters/{id}/redrive`, `POST /inhibitions`, `DELETE /inhibitions/{sourceID}/{targetID}`, `POST`/`PATCH`/`DELETE /maintenance-windows...` | `alerts:write` |
| `GET /stats`, `GET /monitors/{id}/stats` | `stats:read` |
| `GET /stats/alerts-daily` | `alerts:read` |
| `POST /tokens`, `POST /tokens/{id}/revoke` | `tokens:write` |
| `GET /tokens` | `tokens:read` |
| `GET /version`, `GET /poller`, `GET /rule-types` | `none`: any valid credential, including a scoped token with no matching resource scope |
| `GET /audit` | `monitors:read` |
| `/health`, `/livez`, `/readyz` | No credential at all |

A channel **test** is a write even though nothing is stored: it sends a real
notification. Acknowledging an alert is a write because it stops an escalation.

## Roles

SoroBeacon has three roles, `viewer < editor < admin`, and they are
**enforced** on both surfaces. The API router and the dashboard router each
mount the same middleware (`internal/auth.RoleCheck`) over a table of what
each route requires (`internal/api/roles.go`, `internal/web/roles.go`).

### Where a role comes from

| Credential | Role |
| --- | --- |
| Static bearer token `API_TOKEN=secret` | `admin` |
| Static bearer token `API_TOKEN=secret:viewer` | `viewer` |
| Static bearer token `API_TOKEN=secret:editor` | `editor` |
| Static bearer token `API_TOKEN=secret:admin` | `admin` |
| Workspace token `WORKSPACE_TOKENS=team=secret:editor` | `editor` |
| Dashboard session minted from a static token | the token's role |
| Dashboard session minted from an OIDC identity | the role the provider's claim maps, else `admin` |
| Scoped bearer token (`sb_` prefix) | the coarsest role its scopes allow: read-only is a `viewer`, a write scope is an `editor`, `tokens:write` is an `admin` |
| No credential configured | `admin` (the instance is open) |

The suffix is **stripped when the token table is built**: an entry
`secret:viewer` is stored as the digest of `secret`, so a client presents
`secret`, not `secret:viewer`. The suffix selects the role; it is not part of
the secret the client sends.

An OIDC deployment maps roles with `OIDC_ROLE_CLAIM`, naming a claim in the
verified ID token whose value is `viewer`, `editor` or `admin`. A claim the
token lacks, or one naming no known role, leaves the identity an admin.

### What each role may do

* A **viewer** reads. It cannot change the instance.
* An **editor** changes what the instance is for: monitors, rules, channels,
  maintenance windows, saved searches, and the alerts that flow through them
  (acknowledging, retrying a delivery, redriving a dead letter, ingesting a
  federated alert).
* An **admin** does the administrative things on top: minting and revoking
  API tokens, and reading the audit log.

The API route table (`internal/api/roles.go`) is the authority for the API;
the dashboard's (`internal/web/roles.go`) is the authority for the dashboard.
Both are deny-by-default: a route with no entry is refused to every
credential, and a test walks the router and fails if a route and the table
disagree in either direction.

| API routes | Minimum role |
| --- | --- |
| `GET /monitors`, `/monitors/{id}`, `/monitors/{id}/stats`, `/monitors/{id}/rules`, `/monitors/{id}/escalation`, `/monitors/trash` | viewer |
| `GET /channels`, `/channels/{id}`, `/channels/{id}/stats` | viewer |
| `GET /templates`, `/templates/{id}` | viewer |
| `GET /alerts`, `/alerts.csv`, `/alerts/export`, `/alerts/stream`, `/alerts/{id}/deliveries`, `/dead-letters`, `/inhibitions`, `/maintenance-windows`, `/maintenance-windows/{id}` | viewer |
| `GET /stats`, `/stats/alerts-daily` | viewer |
| `GET /version`, `/poller`, `/rule-types` | viewer |
| `POST /monitors`, `/monitors/bulk`, `/monitors/import` | editor |
| `PATCH`/`DELETE /monitors/{id}`, `POST /monitors/{id}/duplicate`, `/soft-delete`, `/restore` | editor |
| `POST /monitors/{id}/rules`, `/rules/bulk`, `PATCH`/`DELETE /monitors/{id}/rules/{ruleID}` | editor |
| `POST /monitors/{id}/rules/dry-run` | viewer |
| `PUT`/`DELETE /monitors/{id}/escalation` | editor |
| `POST /channels`, `PATCH`/`DELETE /channels/{id}`, `/channels/bulk`, `/channels/{id}/duplicate`, `/channels/{id}/test` | editor |
| `POST /templates`, `PATCH`/`DELETE /templates/{id}`, `/templates/{id}/instantiate`, `/templates/{id}/instantiate/bulk` | editor |
| `POST /alerts/{id}/acknowledge`, `/alerts/{id}/deliveries/{channelID}/retry`, `/ingest`, `/dead-letters/{id}/redrive`, `/inhibitions`, `DELETE /inhibitions/{sourceID}/{targetID}` | editor |
| `POST`/`PATCH`/`DELETE /maintenance-windows...` | editor |
| `GET`/`POST /tokens`, `POST /tokens/{id}/revoke` | admin |
| `GET /audit` | admin |
| `/health`, `/livez`, `/readyz` | no credential at all |

The dashboard's table follows the same shape: every page that changes
something is an editor's, the token pages are an admin's, the read-only pages
are a viewer's, and the sign-in flow (`/login`, `/logout`, the OIDC paths,
`/theme`, `/timezone`, `/favicon.ico`) needs no credential.

### Roles and scopes together

A request has to pass **both** gates. The role says how far the credential
reaches; the scopes say which resources within that reach it may touch. A
scoped token with `monitors:read` is a viewer, so it may read monitors, and
the scope table is what stops it reading channels. A scoped token with
`tokens:write` is an admin, so it may mint tokens — and the scope table
still requires `tokens:write` for the token routes.

### What this changed for an existing deployment

Roles were defined but not enforced before this shipped. **A static token
without a role suffix is still an admin**, so an existing deployment sees no
change; a token that *did* carry a `:viewer` suffix now really is a viewer
and can no longer write. If anyone relied on such a token to change things,
give them `:editor` or `:admin` instead. See
[Upgrading](../operations/upgrading.md#roles-are-enforced-now).

## Workspaces

The credential chooses the workspace; a request cannot ask for a different one.
The store applies the workspace as a SQL predicate on every tenant table
(`internal/store/workspace_scope.go`).

* With no `WORKSPACE_TOKENS`, everything is the default workspace.
* `WORKSPACE_TOKENS` maps each token to one workspace. The same token under two
  workspaces, or an invalid workspace id, fails startup.
* A scoped token belongs to the workspace recorded on its row.
* A dashboard session inherits the workspace of the credential that signed in;
  an OIDC session uses `OIDC_WORKSPACE` or a configured claim.
* Background work (polling, retention, reorg handling) runs under a system
  scope and sees every workspace by design. Tenancy is a row filter in one
  process, not a sandbox; see
  [Securing a deployment](../operations/security.md#workspaces-what-isolation-does-and-does-not-give-you).

## Surface by surface

Authorization is **not uniform** across the listeners. This is the part most
likely to surprise an operator.

| Surface | Authentication | Authorization |
| --- | --- | --- |
| REST `/api/v1` | Yes, when configured | Roles on every route, plus route scopes for `sb_` tokens |
| Dashboard `/` | Yes, when configured; session cookie | Roles on every page |
| gRPC (when `GRPC_ADDR` is set) | `authorization` metadata, **static tokens only** | **None**; reads use the default workspace |
| GraphQL `/graphql` | **None** | None; the endpoint is currently a fixed schema stub |
| `/metrics` | **None** | None; mounted on the root router before auth middleware |
| `/health`, `/livez`, `/readyz` | **None** by design | None |

### gRPC

The gRPC server exposes read-only RPCs (`GetMonitor`, `ListMonitors`,
`ListAlerts`, `StreamAlerts`) and verifies the `authorization` metadata with the
same authenticator (`internal/grpc/server.go`). But:

* It calls `Verify`, which resolves **static tokens only**. A scoped `sb_`
  token cannot call gRPC, regardless of its scopes.
* It does not resolve or inject a workspace. A static token bound to a named
  workspace authenticates and then reads the **default** workspace, including
  data belonging to another tenant. Treat gRPC as single-tenant until this is
  fixed, and do not expose the port across a tenancy boundary.
* When no credential source is configured, gRPC is open like the REST API.

### GraphQL

`/graphql` is mounted on the root router **outside** the `/api/v1` auth
middleware (`cmd/sorobeacon/main.go`, `internal/api/graphql/resolver.go`). The
current handler does not execute the real resolvers: it answers every request
with a fixed `{"data":{"__schema":{"queryType":{"name":"Query"}}}}` and `200`.
It exposes no data today, but it is unauthenticated and should not be treated
as a data surface or exposed publicly. The optional playground at
`/graphql/playground` is likewise unauthenticated.

### Probes and metrics

`/api/v1/health`, `/livez` and `/readyz` are exempt from authentication so
orchestrators can check them; `/readyz` reports per-dependency error detail.
`/metrics` is mounted on the root router before the API middleware and is also
unauthenticated. Put these behind the network boundary described in
[Securing a deployment](../operations/security.md#what-to-put-in-front-of-it).

## With authentication disabled

`API_TOKEN`, `WORKSPACE_TOKENS` and `OIDC_ISSUER` unset means the HTTP listener
is **open**. Both middlewares step aside, every request acts on the default
workspace, and the process logs one startup warning. Anyone who can reach the
port can:

* create, rewrite and delete monitors, rules and channels;
* change where alerts are delivered;
* read alert history and delivery records;
* trigger channel test deliveries, which use the stored credential;
* change maintenance windows and inhibitions.

Channel `config` values are never returned by the API, but the channel
**management** API accepts them and can redirect deliveries. This posture is
deliberate for the docker-compose quickstart; it is not a posture for anything
reachable beyond a trusted network. For a public deployment, set `API_TOKEN`
and put a TLS-terminating, authenticating boundary in front of the listener.

## Choosing a credential

| Caller | Use | Why |
| --- | --- | --- |
| Operator on the dashboard | `API_TOKEN` sign-in, or SSO | Full reach; sessions expire after 12 hours |
| A script or CI job that must create monitors | Scoped token (`monitors:read`, `monitors:write`) | Least privilege; expires; revocable without a restart |
| A service that only reads alerts | Scoped token (`alerts:read`) | Cannot mutate or redirect deliveries |
| A gRPC client | Static token | Scoped tokens do not authenticate over gRPC |
| An orchestrator health check | No credential | The probes are exempt by design |

Do not give a CI job `API_TOKEN`: it is the operator credential, never expires,
and rotating it means editing the environment and restarting every replica.

## Rotation and revocation

**Static tokens** (`API_TOKEN`, `WORKSPACE_TOKENS`):

* `API_TOKEN` and `WORKSPACE_TOKENS` are comma-separated lists, so rotation is
  add the new value, roll clients over, remove the old value.
* Revocation is editing the environment and restarting or redeploying. There is
  no per-token revocation endpoint for static values.
* Changing them requires a restart; existing dashboard sessions survive a
  restart only if the process does not restart (sessions are in memory), so in
  practice a restart logs everyone out.

**Scoped tokens**:

* Revocation is immediate: the middleware resolves the `sb_` token to a
  database row on every request (`POST /tokens/{id}/revoke`, or the same action
  on the dashboard).
* The revoked row survives with `revoked_at` set, preserving the audit trail.
  Revoking twice keeps the first timestamp.
* Expiry is set at mint time (`expires_in`, a Go duration such as `72h`); an
  expired token returns the same `401` as a wrong one.
* Only a SHA-256 digest of the secret is stored. A lost token cannot be
  recovered from the database or the dashboard; mint a new one and revoke the
  old one.
* `last_used_at` is updated at most once a minute; treat it as "recently used",
  not as an exact audit timestamp.

**Dashboard sessions**:

* 12-hour TTL, held in memory. A restart logs everyone out.
* Signing out drops that session; deactivating an OIDC account stops the next
  login but does not end an existing session.
* A session carries no scope list and is unrestricted within its workspace. A
  scoped token cannot sign in at `/login`, because a browser cookie that is
  neither a token nor an SSO identity must not silently become one.

## Known gaps worth knowing

These are behaviours in the current code, not recommendations. They are listed
here because a reader of an authorization reference should not discover them
from an incident.

* **REST token management returns `503` in the shipped binary.** The dashboard
  is given the token manager; the API server in `cmd/sorobeacon/main.go` is
  not, so `POST/GET /api/v1/tokens` and revoke answer
  `503 "token management is not configured"` even though the routes and scopes
  exist. Mint and revoke from the dashboard until the wiring is corrected.
* **Roles are enforced on the REST API and the dashboard, but not on gRPC or
  GraphQL.** See [Roles](#roles).
* **gRPC reads the default workspace** for every caller, including a token
  bound to another workspace.
* **GraphQL is unauthenticated** and currently a fixed stub.
* **`/metrics` and the probes are unauthenticated**; `/readyz` includes
  dependency error detail.
* **A scoped token with `tokens:write` cannot mint `*`**, by design, but it can
  mint any scope it already holds. Treat the ability to mint as equivalent to
  the ability to extend a credential's lifetime within that grant.
