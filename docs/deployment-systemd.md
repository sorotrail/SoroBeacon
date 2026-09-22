# Deploying SoroBeacon with systemd

Docker Compose is the quickest way to try SoroBeacon. For a single Go
binary on a VPS, systemd is the usual supervisor: it restarts the process
after a crash, ships stdout into the journal, and can wait for Postgres
before starting.

This page is a copy-paste path from a built binary to a running unit.
It does not cover Docker, Kubernetes, or reverse-proxy TLS — only the
systemd unit and the files around it.

Channel `config` in the database holds webhook URLs, bot tokens and SMTP
credentials. The environment file holds `DATABASE_URL` and any RPC
credentials. Never paste those into a ticket, a screenshot, or a
`journalctl` snippet you share.

## What you need

- A Linux host with systemd (Debian/Ubuntu, Fedora, or similar).
- Postgres reachable from that host. The unit waits on
  `postgresql.service` when Postgres is local; for a remote database
  `network-online.target` is the relevant dependency.
- Go 1.25+ if you build on the host, or a binary you fetched from CI.

SoroBeacon runs migrations automatically on startup (`store.Migrate` in
`cmd/sorobeacon`). There is no separate migrate step to wire into the
unit — if the process starts, the schema is already at HEAD or it
exited.

Logs are JSON on stdout (`slog.NewJSONHandler`). systemd captures that
into the journal.

## 1. Dedicated user

Do not run the binary as root. Create a system user with no login shell
and a home used only as the working directory:

```sh
sudo useradd --system --home-dir /var/lib/sorobeacon --create-home \
  --shell /usr/sbin/nologin sorobeacon
```

On Alpine-style hosts the equivalent is `adduser -S -H -h /var/lib/sorobeacon
-s /sbin/nologin sorobeacon` plus `mkdir`/`chown` of the home.

## 2. Binary

Build on the host:

```sh
git clone https://github.com/sorotrail/SoroBeacon.git
cd SoroBeacon
make build          # writes ./bin/sorobeacon
sudo install -o root -g root -m 0755 bin/sorobeacon /usr/local/bin/sorobeacon
```

Or copy a CI artifact to the same path. Confirm it is the build you
expect:

```sh
/usr/local/bin/sorobeacon -h 2>/dev/null || true
# version is also at GET /api/v1/version once the service is up
```

The binary is statically built in the Docker image (`CGO_ENABLED=0`); a
local `make build` is the same command the Makefile uses.

## 3. Environment file

All configuration is environment variables. Put them in a file that
**root owns, mode `0600`**. systemd reads it as `EnvironmentFile=` before
dropping to `User=sorobeacon`. If the file is group- or world-readable,
the database URL and any channel-adjacent secrets on disk are exposed to
other users on the box.

```sh
sudo mkdir -p /etc/sorobeacon
sudo install -o root -g root -m 0600 /dev/null /etc/sorobeacon/sorobeacon.env
sudo editor /etc/sorobeacon/sorobeacon.env
```

Minimum contents (see `.env.example` for the full list):

```
DATABASE_URL=postgres://sorobeacon:REDACTED@127.0.0.1:5432/sorobeacon?sslmode=disable
NETWORK=mainnet
RPC_URL=https://your-mainnet-rpc.example
HTTP_ADDR=:8080
LOG_LEVEL=info
SOURCE_MODE=rpc
```

`DATABASE_URL` is required. `HTTP_ADDR=:8080` binds all interfaces —
put a reverse proxy in front if this host is reachable from the
internet; the API is unauthenticated.

Confirm the permission after editing:

```sh
stat -c '%U:%G %a %n' /etc/sorobeacon/sorobeacon.env
# root:root 0600 /etc/sorobeacon/sorobeacon.env
```

## 4. Unit file

Install as `/etc/systemd/system/sorobeacon.service`:

```ini
[Unit]
Description=SoroBeacon Soroban contract monitor
Documentation=https://github.com/sorotrail/SoroBeacon
# network-online: DNS and a remote RPC/Postgres must be reachable.
# postgresql.service: when Postgres is on this host, wait for it.
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
User=sorobeacon
Group=sorobeacon
WorkingDirectory=/var/lib/sorobeacon
ExecStart=/usr/local/bin/sorobeacon
Restart=on-failure
RestartSec=5s
EnvironmentFile=/etc/sorobeacon/sorobeacon.env

# The process never needs extra capabilities after start.
NoNewPrivileges=true
# Make /usr, /boot, /etc read-only for the service (the env file is
# already loaded; the binary lives under /usr/local/bin).
ProtectSystem=strict
# Private /tmp so a compromised process cannot see other units' temp files.
PrivateTmp=true
# Working directory is otherwise under ProtectSystem=strict.
ReadWritePaths=/var/lib/sorobeacon

# JSON logs on stdout land in the journal.
StandardOutput=journal
StandardError=journal
SyslogIdentifier=sorobeacon

[Install]
WantedBy=multi-user.target
```

`Restart=on-failure` covers crashes and non-zero exits (a bad
`DATABASE_URL` or RPC mismatch fails fast at startup). It does not
restart on a clean `systemctl stop`.

`After=network-online.target` and `After=postgresql.service` order the
unit; they do not fail the start if Postgres is remote and
`postgresql.service` does not exist on this host. systemd ignores
missing `After=` names.

Hardening, one line each:

| Directive | Why |
| --- | --- |
| `NoNewPrivileges=true` | The process cannot regain root via setuid helpers after start. |
| `ProtectSystem=strict` | `/usr`, `/boot` and `/etc` are read-only in the service mount namespace. |
| `PrivateTmp=true` | The unit gets its own `/tmp`, isolated from other services. |

## 5. Enable, start, check

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now sorobeacon.service
sudo systemctl status sorobeacon.service
```

Migrations run during `ExecStart` before the HTTP server listens. A
failure there (bad `DATABASE_URL`, unreachable Postgres) is a non-zero
exit and `Restart=on-failure` will retry.

Health once it is listening:

```sh
curl -s localhost:8080/api/v1/health
curl -s localhost:8080/api/v1/readyz
curl -s localhost:8080/api/v1/version
```

## 6. Logs

```sh
sudo journalctl -u sorobeacon.service -f
```

Logs are JSON on stdout, so `journalctl -o cat` is useful for piping
into other tools (`jq`, `grep` for a `request_id`, shipping to a log
aggregator) without systemd's extra timestamp prefix:

```sh
sudo journalctl -u sorobeacon.service -o cat | jq -c 'select(.msg=="database ready")'
sudo journalctl -u sorobeacon.service -o cat --since "10 min ago"
```

Useful startup lines: `database ready`, then poller/dispatcher activity.
A `fatal` line with `err` is a failed start (config, migrate, or listen).
Never copy channel `config` or `DATABASE_URL` out of a log snippet.

## Updating

```sh
cd /path/to/SoroBeacon
git pull
make build
sudo install -o root -g root -m 0755 bin/sorobeacon /usr/local/bin/sorobeacon
sudo systemctl restart sorobeacon.service
```

Migrations for the new binary still run on startup. Do not edit applied
files under `internal/store/migrations`.

## Related

- [Quickstart](getting-started/quickstart.md) — Docker Compose and a
  local binary without systemd
- [Configuration](getting-started/configuration.md) — every environment
  variable
- [HTTP API](reference/api.md) — health, readyz, stats
