# Matrix

Sends the rendered alert into a Matrix room through the client-server API — the natural chat target for teams who would rather not route contract activity through a commercial SaaS.

## Setup

1. Create a bot user on your homeserver and join it to the room you want alerts in.
2. Get an access token for the bot (e.g. `POST /_matrix/client/v3/login`, or an application service token).
3. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "ops-matrix",
  "type": "matrix",
  "config": {
    "homeserver_url": "https://matrix.example.org",
    "access_token": "syt_...",
    "room_id": "!abcdef:example.org"
  }
}'
```

4. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/3/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `homeserver_url` | yes | Base URL of the homeserver, e.g. `https://matrix.example.org`. |
| `access_token` | yes | Bot access token. Treated as a secret. |
| `room_id` | yes | Room to post into, e.g. `!abcdef:example.org`. |

## Delivery semantics

The message is sent with `PUT /_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}`, where `txnId` is derived from the alert ID. A retried delivery reuses the transaction ID, so the homeserver returns the original event instead of posting the message twice. Non-2xx responses count as failures and are retried with backoff.

The access token travels in the `Authorization` header, never in the URL, and is never included in error messages or delivery `response_snippet`s.
