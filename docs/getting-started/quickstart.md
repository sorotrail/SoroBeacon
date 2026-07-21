# Quickstart

## With Docker (recommended)

```sh
git clone https://github.com/khaylebfortune/sorobeacon.git
cd sorobeacon
docker compose up --build -d
```

That starts Postgres and SoroBeacon against the Stellar **testnet** RPC. Database migrations run automatically on startup.

* Dashboard: [http://localhost:8080](http://localhost:8080)
* API: `http://localhost:8080/api/v1`

Verify it's healthy:

```sh
curl -s localhost:8080/api/v1/health
# {"db":"ok","rpc":"ok","rpc_latest_ledger":3721688,"status":"ok"}
```

## Without Docker

You need Go 1.25+ and a Postgres you can reach.

```sh
cp .env.example .env        # edit DATABASE_URL
make build
set -a; . ./.env; set +a
./bin/sorobeacon
```

## First alert in four requests

```sh
# 1. A channel to deliver to (Discord here; see the channel reference for others)
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops", "type": "discord",
  "config": {"webhook_url": "https://discord.com/api/webhooks/..."}
}'

# 2. A monitor watching your contract, wired to that channel
curl -s -X POST localhost:8080/api/v1/monitors -d '{
  "name": "My token",
  "contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
  "channel_ids": [1]
}'

# 3. A rule: alert on every transfer event
curl -s -X POST localhost:8080/api/v1/monitors/1/rules -d '{
  "type": "event_emitted",
  "params": {"event_name": "transfer"}
}'

# 4. Prove the channel works right now
curl -s -X POST localhost:8080/api/v1/channels/1/test
```

From here, every `transfer` the contract emits lands in your Discord within one poll interval (5 seconds by default).

{% hint style="info" %}
On first start SoroBeacon begins at the **current ledger tip** — it does not (and cannot) backfill history. After a restart it resumes from its checkpoint.
{% endhint %}

## Mainnet

Point `RPC_URL` at a mainnet Stellar RPC endpoint (self-hosted or a provider) — see [Configuration](configuration.md). Everything else is identical.
