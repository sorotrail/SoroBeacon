# Pushover

Sends alerts as push notifications to phones and desktops through
[Pushover](https://pushover.net). It is the simplest way for a solo operator
or small team to get "ping my phone when this contract does something"
without running a paging rotation.

The channel calls Pushover's
[messages API](https://pushover.net/api) (`POST /1/messages.json`) with a
form-encoded body.

## Setup

1. Sign in at [pushover.net](https://pushover.net) and copy your **User Key**
   from the dashboard (or create a **Delivery Group** and use its group key to
   notify several people).
2. **Create an Application/API Token** — name it *SoroBeacon* — and copy the
   application **API Token**.
3. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "my-phone",
  "type": "pushover",
  "config": {
    "token": "<application token>",
    "user": "<user or group key>",
    "priority": 0,
    "device": "phone"
  }
}'
```

4. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/<id>/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `token` | yes | Application API token. Treated as a secret: it never appears in errors, logs or delivery records. |
| `user` | yes | User key or delivery-group key. Treated as a secret. |
| `priority` | no | Notification priority, `-2` to `1` (see below). Defaults to `0`. Any other value is rejected when the channel is created. |
| `device` | no | Send only to this device name (as registered in the Pushover app) instead of all of the user's devices. |
| `template` | no | Go `text/template` overriding the message body. See [Message templates](templates.md). |

### Priority levels

| Value | Name | Behaviour |
| --- | --- | --- |
| `-2` | Lowest | No notification at all; the message only appears in the app. |
| `-1` | Low | Delivered quietly: no sound or vibration. |
| `0` | Normal | Default. Sound and vibration, respecting the user's quiet hours. |
| `1` | High | Bypasses the user's quiet hours; highlighted in red in the app. Use it for monitors that must reach you at night. |

Priority `2` (emergency, repeated until acknowledged) needs retry/expire
parameters and receipt tracking, and is not supported by this channel.

## Message format

- **Title:** `SoroBeacon: <monitor name>`.
- **Body:** the standard alert summary — rule type, contract ID, ledger,
  transaction, event ID and time — or your `template` output.

Pushover limits titles to 250 characters and messages to 1024 characters.
Longer values are truncated to fit (ending in `…`) rather than rejected, so
an unusually long alert still arrives.

## Failure behaviour

A non-2xx response becomes a delivery error of the form
`pushover: status 400: <truncated response body>`. Pushover's error bodies
describe which field was wrong (e.g. `"token":"invalid"`) without echoing the
value. Network errors are reported with the URL stripped.
