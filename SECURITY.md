# Security policy

SoroBeacon monitors Soroban contracts and delivers alerts. Its security
posture is unusual and worth stating plainly: the API and dashboard are
**unauthenticated by design** in the MVP, and the database holds channel
secrets (webhook URLs, bot tokens, SMTP credentials) in plaintext.

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

- **No API authentication.** Run on a trusted network or behind a
  reverse proxy with auth.
- **Channel secrets stored unencrypted.** A database compromise yields
  every channel credential. Encrypting at rest is an open issue.
- **Channel secrets in `config` JSON** are never logged and never
  returned by the API, but they exist in the database and in the
  request body that created them.
