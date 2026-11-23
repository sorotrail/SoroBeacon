# External secrets in channel config and configuration

Channel config holds webhook URLs, bot tokens and SMTP passwords. Core configuration carries database credentials, encryption keys, and tokens. By default
those values live in SoroBeacon's database (encrypted at rest when
`CONFIG_ENCRYPTION_KEY` is set) or environment variables. External secrets let you keep the value in a
system you already audit and rotate — Vault, Kubernetes projected files, your process environment — and
store only a *reference* in the configuration or channel config.

## Reference syntax

```
${secret:<provider>:<path>[#<key>]}
```

| Part       | Meaning                                                                 |
|------------|-------------------------------------------------------------------------|
| `provider` | The configured provider scheme: `env`, `vault`, or `file`.              |
| `path`     | Provider-specific location of the secret.                               |
| `key`      | Optional field name inside a structured secret.                         |

A config value that is **exactly** a reference is resolved when configuration is loaded (for startup settings like `DATABASE_URL` or `API_TOKEN`) or when the notifier is constructed (at send time for channels). Anything else — a plain URL, a string with surrounding text, a malformed reference — is treated as a literal, so existing deployments keep working untouched.

```json
{
  "webhook_url": "${secret:vault:kv/data/sorobeacon#slack_webhook_url}",
  "channel": "#ops"
}
```

Resolved values are never written back to the database or written to disk. The stored config keeps the reference; only the in-memory notifier or configuration sees the secret. Resolved values are never logged, returned by the API, echoed in errors, or included in delivery `response_snippet`s. A resolution failure names the reference — never the value — and fails startup or delivery rather than delivering an unresolved placeholder.

## Enabling a provider

Nothing is resolved unless `SECRETS_PROVIDER` names a provider (or comma-separated list of providers); without it references are literals or startup validation errors.

```sh
# Environment variables
SECRETS_PROVIDER=env

# Local file or Kubernetes Secret projected volume
SECRETS_PROVIDER=file

# HashiCorp Vault (KV v1 or v2)
SECRETS_PROVIDER=vault
VAULT_ADDR=https://vault.example:8200
VAULT_TOKEN=hvs....
# VAULT_NAMESPACE=team-a   # Vault Enterprise only

# Multi-provider combination
SECRETS_PROVIDER=file,vault
```

| Variable             | Meaning                                                              |
|----------------------|----------------------------------------------------------------------|
| `SECRETS_PROVIDER`   | `env`, `vault`, `file`, comma-separated combination, or unset.       |
| `SECRETS_CACHE_TTL`  | How long a resolved value is reused. Default `5m`; `0s` disables.    |
| `VAULT_ADDR`         | Vault base URL. Required when `SECRETS_PROVIDER` includes `vault`.   |
| `VAULT_TOKEN`        | Vault token. A credential; never logged.                             |
| `VAULT_NAMESPACE`    | Optional `X-Vault-Namespace` for Vault Enterprise.                   |

## Providers

### `file`

`${secret:file:/var/run/secrets/token}` reads raw file contents, trimmed of leading and trailing whitespace.
`${secret:file:/etc/secrets/config.json#key}` parses the file as JSON and extracts `key`.
`${secret:file:/etc/secrets/app.env#KEY}` parses line-delimited `KEY=VALUE` or `KEY: VALUE` pairs and extracts `KEY`.

Bounded reading limits file size to 1MB to prevent unbounded memory allocation. Ideal for Kubernetes Secret projected volumes and local development.

### `env`

`${secret:env:SLACK_WEBHOOK_URL}` resolves the environment variable
`SLACK_WEBHOOK_URL`. If a key is given (`${secret:env:unused#SLACK_WEBHOOK_URL}`)
it names the variable instead, which suits configs generated from a template
that always emits a key.

### `vault`

`${secret:vault:kv/data/sorobeacon#slack_token}` reads `/v1/kv/data/sorobeacon`
from `VAULT_ADDR` and returns the `slack_token` field. Both KV v1
(`{"data":{...}}`) and KV v2 (`{"data":{"data":{...}}}`) responses are
understood. Without a key, the conventional `value` field is used, or the
single field when the secret has exactly one.

## Secret inventory and rotation

| Secret | Location | Resolution Path | Rotation Without Restart |
|---|---|---|---|
| Channel Configs (Slack, Telegram, Discord, PagerDuty, SMTP, Webhooks) | `channels.config` JSON | Send-time via `secrets.Resolver` with `SECRETS_CACHE_TTL` | **Yes** (Picked up within TTL or on channel mutation) |
| Primary Database URL | `DATABASE_URL` | Startup via `secrets.Resolver` | **No** (pgxpool connection pool established at boot) |
| Read-Replica Database URL | `REPLICA_DATABASE_URL` | Startup via `secrets.Resolver` | **No** (Replica connection pool established at boot) |
| Config Encryption Key | `CONFIG_ENCRYPTION_KEY` | Startup via `secrets.Resolver` | **No** (AES-GCM cipher initialized at boot) |
| API Static Bearer Tokens | `API_TOKEN` | Startup via `secrets.Resolver` | **Yes** (via comma-separated overlap list) |
| Workspace Bearer Tokens | `WORKSPACE_TOKENS` | Startup via `secrets.Resolver` | **No** (requires restart) |
| OIDC Client Secret | `OIDC_CLIENT_SECRET` | Startup via `secrets.Resolver` | **No** (OIDC client initialized at boot) |
| Dead Man's Snitch URL | `HEARTBEAT_URL` | Startup via `secrets.Resolver` | **No** (Heartbeat client initialized at boot) |
| Vault Provider Token | `VAULT_TOKEN` | Bootstrap env/file | **No** (Bootstrap credential; cannot self-resolve from Vault) |

## Caching

Resolved values are cached for `SECRETS_CACHE_TTL` (default 5 minutes) so a
burst of alerts does not hit the provider once per alert. The cache is
invalidated whenever a channel is created, updated or deleted, so a corrected
reference takes effect on the next delivery. A rotated secret is otherwise
picked up when its cache entry expires; set a shorter TTL if you need faster
propagation.

Invalidation is process-wide: the resolver keeps one cache per process, not one
per channel, so an edit to any channel clears every cached reference. The next
delivery for every channel re-resolves its references once.

## Adding a provider

`internal/secrets.Provider` is two methods:

```go
type Provider interface {
    Scheme() string
    Fetch(ctx context.Context, path, key string) (string, error)
}
```

Implement it, register it on a `secrets.Resolver` in `buildSecretResolver`
(`cmd/sorobeacon/main.go`), and references using its scheme resolve. The test
suite's fake provider shows the whole surface a new provider needs.
