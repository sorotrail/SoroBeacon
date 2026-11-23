# ntfy

Posts alert messages to a topic on [ntfy.sh](https://ntfy.sh) or a
self-hosted ntfy server. ntfy is a good fit when you want phone push
notifications without Discord or Slack — and, if you run the server
yourself, without a third party seeing your alerts.

## Setup

1. Install the ntfy app (iOS, Android, web) or open the web UI, and
   subscribe to a topic name. Topic names are shared secrets: anyone who
   knows yours can read and publish to it, so pick something unguessable
   (or self-host and put ntfy behind auth).
2. For a protected topic or a self-hosted server with `auth-default-access`
   enabled, create an access token in the ntfy web UI and copy it.
3. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "oncall-ntfy",
  "type": "ntfy",
  "config": {"topic": "sorobeacon-8f3a1c", "access_token": "tk_abc123"}
}'
```

4. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/4/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `topic` | yes | Topic to publish to. Treated as a secret. |
| `server_url` | no | Base URL of the ntfy server. Defaults to `https://ntfy.sh`; a trailing slash is ignored. |
| `access_token` | no | Bearer token sent in the `Authorization` header. Treated as a secret. |
| `priority` | no | Notification priority: `1`–`5`, or `min`, `low`, `default`, `high`, `max`, `urgent`. Omit for the server default. |

## Self-hosted and private networks

`server_url` accepts plain `http://` as well as `https://`. Self-hosted
ntfy instances are commonly reached over a private network or a VPN where
TLS is terminated elsewhere (or not used at all), and requiring `https://`
would make the common case impossible. Do not send an access token over
plain `http://` on an untrusted network — put the server behind TLS, or
reach it over a private link.

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "homelab-ntfy",
  "type": "ntfy",
  "config": {"server_url": "http://ntfy.lan:2586", "topic": "sorobeacon"}
}'
```

Messages use the same plain-text alert summary as every chat channel (see
[Discord](discord.md) for a sample); the monitor name is sent as the
notification's `Title`.
