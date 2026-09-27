# Security policy

SoroBeacon monitors Soroban contracts and delivers alerts. Its security
posture is unusual and worth stating plainly: with `API_TOKEN` unset the API
and dashboard are **open to anyone who can reach the port**, and the database
holds channel secrets (webhook URLs, bot tokens, SMTP credentials) in
plaintext. Authentication is a single static token, not user accounts.

## Reporting a vulnerability

Email security concerns to the repository owner via the GitHub
"Contact" link on the profile (sorotrail). Please do not open a public
issue for anything you believe is exploitable.

Include reproduction steps and, where relevant, the request that
triggers it. A proof of concept is welcome but not required.

## Scope

In scope:

- The HTTP API and dashboard, including anything reachable through
  unauthenticated requests
- The alert delivery path: how channel secrets are handled, logged and
  exposed
- The ingest path against a malicious or misbehaving RPC endpoint

Out of scope:

- Deployments that expose the API to untrusted networks without their
  own access control — the README is explicit that this is unsupported
- Vulnerabilities in dependencies, unless triggerable through SoroBeacon
  in a way a patch here would fix

## Known limitations, tracked openly

These are accepted risks with contributor issues, not surprises:

- **One static credential, all-or-nothing.** `API_TOKEN` gates `/api/v1`
  and the dashboard sign-in; there are no accounts, no roles and no per-user
  audit trail, and anyone holding a token can read and mutate everything.
  Unset, the deployment is open. Set it, and keep the token out of tickets,
  logs and shell history.
- **Probes and metrics are unauthenticated.** `/health`, `/livez` and
  `/readyz` are exempt so orchestrators keep working, and `/metrics` is served
  outside the authenticated routers. Keep both off the public internet.
- **Channel secrets are plaintext unless `CONFIG_ENCRYPTION_KEY` is set.**
  With it set, `channels.config` is encrypted at rest and a database or
  backup compromise yields ciphertext. Left unset — the default so existing
  deployments upgrade safely — secrets are readable in the database and
  backups. The key itself is a credential: losing it makes encrypted rows
  unrecoverable, so back it up with the database.
- **Channel secrets in `config` JSON** are never logged and never
  returned by the API, but they exist in the database and in the
  request body that created them.
