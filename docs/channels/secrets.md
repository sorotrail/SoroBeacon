# External secrets in channel config

Channel config holds webhook URLs, bot tokens and SMTP passwords. By default
those values live in SoroBeacon's database (encrypted at rest when
`CONFIG_ENCRYPTION_KEY` is set). External secrets let you keep the value in a
system you already audit and rotate — Vault, your process environment — and
store only a *reference* in the channel config.

## Reference syntax

```
${secret:<provider>:<path>[#<key>]}
```

| Part       | Meaning                                                                 |
|------------|-------------------------------------------------------------------------|
| `provider` | The configured provider scheme: `env` or `vault`.                       |
| `path`     | Provider-specific location of the secret.                               |
| `key`      | Optional field name inside a structured secret.                         |

A config value that is **exactly** a reference is resolved when the notifier
is constructed (at send time). Anything else — a plain URL, a string with
surrounding text, a malformed reference — is treated as a literal, so
existing channels keep working untouched.

```json
{
  "webhook_url": "${secret:vault:kv/data/sorobeacon#slack_webhook_url}",
  "channel": "#ops"
}
```

Resolved values are never written back to the database. The stored config
keeps the reference; only the in-memory notifier sees the secret. Resolved
values are never logged, returned by the API, or included in delivery
`response_snippet`s. A resolution failure names the reference — never the
value — and fails that send rather than delivering an unresolved
placeholder.

## Enabling a provider

Nothing is resolved unless `SECRETS_PROVIDER` names a provider; without it
references are literals.

```sh
# Environment variables
SECRETS_PROVIDER=env

# HashiCorp Vault (KV v1 or v2)
SECRETS_PROVIDER=vault
VAULT_ADDR=https://vault.example:8200
VAULT_TOKEN=hvs....
# VAULT_NAMESPACE=team-a   # Vault Enterprise only
```

| Variable             | Meaning                                                              |
|----------------------|----------------------------------------------------------------------|
| `SECRETS_PROVIDER`   | `env`, `vault`, or unset to disable external secrets.                |
| `SECRETS_CACHE_TTL`  | How long a resolved value is reused. Default `5m`; `0s` disables.    |
| `VAULT_ADDR`         | Vault base URL. Required when `SECRETS_PROVIDER=vault`.              |
| `VAULT_TOKEN`        | Vault token. A credential; never logged.                             |
| `VAULT_NAMESPACE`    | Optional `X-Vault-Namespace` for Vault Enterprise.                   |

## Providers

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

## Caching

Resolved values are cached for `SECRETS_CACHE_TTL` (default 5 minutes) so a
burst of alerts does not hit the provider once per alert. The cache is
invalidated whenever a channel is created, updated or deleted, so a corrected
reference takes effect on the next delivery. A rotated secret is otherwise
picked up when its cache entry expires; set a shorter TTL if you need faster
propagation.

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
