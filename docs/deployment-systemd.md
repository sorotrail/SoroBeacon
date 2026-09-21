# Deploying SoroBeacon with systemd

This guide runs a compiled SoroBeacon binary as a dedicated, non-root service
on a Linux host. PostgreSQL must be reachable from the host before starting
the service; SoroBeacon applies its embedded migrations during startup.

## 1. Create the service account and directories

```sh
sudo useradd --system --home-dir /var/lib/sorobeacon --create-home \
  --shell /usr/sbin/nologin sorobeacon
sudo install -d -o sorobeacon -g sorobeacon -m 0750 /opt/sorobeacon/bin
sudo install -d -o sorobeacon -g sorobeacon -m 0750 /var/lib/sorobeacon
```

Build the release binary from a tagged checkout and install it:

```sh
make build
sudo install -o sorobeacon -g sorobeacon -m 0755 \
  bin/sorobeacon /opt/sorobeacon/bin/sorobeacon
```

## 2. Store configuration separately

Create a root-owned environment file. It contains the database URL and channel
secrets, so keep it unreadable by other users:

```sh
sudo install -o root -g sorobeacon -m 0640 /dev/null \
  /etc/sorobeacon.env
sudoedit /etc/sorobeacon.env
```

At minimum, set `DATABASE_URL`. For a non-default network also set
`NETWORK`, `RPC_URL`, and `NETWORK_PASSPHRASE`; see the
[configuration reference](getting-started/configuration.md) for the complete
list. Do not put secrets in the unit file or commit this environment file.

## 3. Install the unit

Create `/etc/systemd/system/sorobeacon.service`:

```ini
[Unit]
Description=SoroBeacon Soroban contract monitoring
Wants=network-online.target
After=network-online.target postgresql.service

[Service]
Type=simple
User=sorobeacon
Group=sorobeacon
WorkingDirectory=/var/lib/sorobeacon
EnvironmentFile=/etc/sorobeacon.env
ExecStart=/opt/sorobeacon/bin/sorobeacon
Restart=on-failure
RestartSec=5s

# The process only needs its configured working directory and network access.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/sorobeacon

[Install]
WantedBy=multi-user.target
```

`NoNewPrivileges` prevents privilege escalation, `ProtectSystem` makes the
host filesystem read-only to the service, and `PrivateTmp` isolates temporary
files. `ReadWritePaths` leaves only SoroBeacon's state directory writable.

Reload systemd, enable the service, and start it:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now sorobeacon.service
sudo systemctl status sorobeacon.service
```

## 4. Verify startup and logs

SoroBeacon checks the configured network at startup and exits if the RPC
endpoint does not match the selected network. Migrations are applied before
the service begins serving requests.

```sh
curl --fail http://127.0.0.1:8080/api/v1/readyz
curl --fail http://127.0.0.1:8080/api/v1/version
sudo journalctl -u sorobeacon.service -n 100 --no-pager
sudo journalctl -u sorobeacon.service -f -o cat
```

Logs are structured JSON on stdout. `journalctl -o cat` removes journald's
prefix when piping the records to a JSON-aware collector. If the service is
restarting, inspect the journal first; common causes are an unreachable
PostgreSQL URL, an RPC/network mismatch, or a port already bound on
`HTTP_ADDR`.

## 5. Updating the binary

Build and install the new binary, then restart the service. Keep the previous
binary available until the new version has passed its health checks so a
rollback is straightforward:

```sh
sudo cp /opt/sorobeacon/bin/sorobeacon /opt/sorobeacon/bin/sorobeacon.previous
make build
sudo install -o sorobeacon -g sorobeacon -m 0755 \
  bin/sorobeacon /opt/sorobeacon/bin/sorobeacon
sudo systemctl restart sorobeacon.service
sudo systemctl is-active --quiet sorobeacon.service
```

