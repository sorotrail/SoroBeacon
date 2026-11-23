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

## Dependency scanning

Every pull request and every push to `main` runs `govulncheck` over the root
module **and** the nested Terraform provider module, which has its own
dependency tree and would otherwise be invisible to the scan. The job is
separate from lint so the pull request page distinguishes a code smell from a
known vulnerability, and the tool version is pinned in the `Makefile` so an
unrelated change cannot fail on the day the vulnerability database updates.
`make vulncheck` reproduces the CI job locally. Only vulnerabilities reachable
from compiled code fail the build; an advisory against an unused path is
reported by the database but is not treated as a release blocker here.

## Static security analysis

Dependency scanning answers whether a third-party library carries a known
advisory; static analysis answers whether **this repository's own Go code**
introduces an exploitable pattern. Two complementary gates run on every pull
request and every push to `main`, across both the root module and
`terraform-provider-sorobeacon`:

1. **CodeQL (`.github/workflows/codeql.yml`, `.github/codeql/codeql-config.yml`).**
   Runs GitHub's `security-extended` Go query suite in `manual` build mode so
   both modules are compiled and extracted. Results are uploaded to GitHub Code
   Scanning (`security-events: write`) and posted inline on the pull request
   diff. Because the workflow reads no secrets and uses `pull_request` rather
   than `pull_request_target`, pull requests from forks run the exact same
   analysis safely. A weekly scheduled run catches newly added upstream queries
   against `main`.
2. **Tuned `gosec` CI gate (`security` job in `.github/workflows/ci.yml`, `.golangci-security.yml`).**
   Runs `golangci-lint` with `default: none` and only `gosec` enabled, scoped
   to the rule classes that match SoroBeacon's attack surface:
   - **SQL construction (`G201`, `G202`):** SQL built by `fmt.Sprintf` or string
     concatenation across the Postgres and SQLite store backends.
   - **Server-side request forgery (`G107`):** Outbound HTTP requests constructed
     from variable URLs on the notification delivery, enrichment, heartbeat, and
     federation paths.
   - **Path traversal & archive extraction (`G304`, `G305`):** Variable file
     paths in config loading, TLS certificate reloaders, and alert archive
     tiering (`file://`, `dir://`).
   - **Decompression bounds (`G110`):** Request body `gzip`/`deflate`
     decompression.
   - **Template escaping (`G203`):** Unescaped data passed to `html/template` in
     the dashboard or notification templates.
   - **Weak randomness & cryptography (`G402`, `G404`, `G501`, `G505`):** Use of
     `math/rand` where `crypto/rand` is required, weak TLS settings, or broken
     hashes (`md5`, `sha1`).
   - **Unsafe memory/reflection (`G103`):** Use of package `unsafe`.

### Handling findings

- **New findings fail the pull request** and appear as inline annotations on the
  changed lines.
- **True positives** become dedicated issues (or targeted fixes) rather than
  being quietly suppressed. In particular, outbound notification and webhook
  senders fetch operator-configured URLs by design; any architectural egress
  control change is tracked as its own issue.
- **False positives and intentional patterns** (for example, jittered retry
  backoff using `math/rand/v2`, or SQL query builders that interpolate only
  internal column/table constants alongside `$1` placeholders) must be
  suppressed **next to the code they excuse** with an explicit rationale:
  ```go
  //nolint:gosec // G404: retry jitter is not security-sensitive; tokens use crypto/rand
  ```
- **No blanket ignores.** `.golangci-security.yml` defines no rule-level text
  exclusions, and `TestGosecSuppressionsCarryAReason` (`internal/ci/security_test.go`)
  fails the test suite if any `//nolint:gosec` or `//#nosec` directive in
  non-test code omits an explanatory `// <reason>` comment.

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
