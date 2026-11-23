# Mobile push (FCM)

The `push` channel delivers an alert to the phones of registered responders
through Firebase Cloud Messaging (HTTP v1). FCM reaches Android directly and
iOS through APNs. FCM is the only platform in this version; the platform
specific code is confined to `internal/notify/push.go`.

## Configuration

```json
{
  "project_id": "my-firebase-project",
  "client_email": "svc@my-firebase-project.iam.gserviceaccount.com",
  "private_key": "${secret:fcm/private_key}",
  "link_base_url": "https://sorobeacon.example.com"
}
```

`private_key` is the service account key (PEM). It resolves through the same
secret machinery as every other channel secret (see [secrets](secrets.md)).
SoroBeacon signs an RS256 assertion with it, exchanges it for a short lived
access token, and caches that token across deliveries. The service account
needs the `Firebase Cloud Messaging API (V1)` send permission only.

## Device registry

Devices belong to a channel. A device id is chosen by the client.

```
PUT    /api/v1/channels/{id}/devices/{deviceID}   {"token": "...", "platform": "fcm"}
GET    /api/v1/channels/{id}/devices
DELETE /api/v1/channels/{id}/devices/{deviceID}
```

`PUT` registers and refreshes: the same device id with a new token replaces the
old one. Device tokens are credentials. They are stored, used for delivery, and
never returned by any API response, written to a log or put in an error. Only
the device id, platform and token are stored. Tokens are not part of workspace
exports.

## Expired tokens

FCM v1 answers an unknown or expired token synchronously (`UNREGISTERED` or
404) on the send itself. SoroBeacon retires that device immediately and does
not try it again; re-registering the device id revives it. There is no
asynchronous feedback feed to poll. If every device fails, the delivery fails
and feeds channel health like any other channel.

## Severity mapping

| Alert severity | Android priority | APNs priority | iOS interruption level |
|---|---|---|---|
| critical | high | 10 | critical (critical alert sound) |
| warning (and unset) | high | 10 | time-sensitive |
| info | normal | 5 | passive |

Only `critical` alerts use the critical class, which bypasses silent mode and
Do Not Disturb. Marking everything critical is abusive and risks Apple revoking
the app's critical-alert entitlement, so keep your rule severities honest. The
iOS app needs that entitlement from Apple for the critical sound to work.

## Payload

The push carries a title (monitor name), a one line body (event, shortened
contract id, ledger) and a data map with the alert id, severity and, when
`link_base_url` is set, a link to `/alerts/{id}`. It never contains the event
payload. Text is summarised to fit well below FCM's 4096 byte limit.

## Rate limits

Per device: a burst of 6, then 6 per minute. Per workspace: a burst of 60, then
60 per minute. Limits are held in memory per process; a limited device is
skipped and, if nothing else was sent, the delivery reports `rate limited`.

## Limitations

- FCM only. No direct APNs, Huawei or web push.
- Limits are per process, not shared across replicas.
- Customising the push text with a template is not supported yet.
