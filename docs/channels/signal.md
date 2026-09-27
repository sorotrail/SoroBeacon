# Signal

Posts alert messages to Signal contacts or groups via a self-hosted
[signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api) instance.

This channel is designed for teams that want alerts on a phone without routing
contract activity through a corporate SaaS. The signal-cli-rest-api bridge keeps
the whole path under the operator's control, matching the self-hosting posture
of the rest of SoroBeacon.

Because the bridge is usually reached over a private network, plain `http://`
is allowed here (unlike the public webhook channels).

## Setup

1. **Deploy signal-cli-rest-api** (Docker example):

   ```sh
   mkdir -p ~/.local/share/signal-api
   docker run -d --name signal-api --restart=always -p 8080:8080 \
     -v ~/.local/share/signal-api:/home/.local/share/signal-cli \
     -e 'MODE=native' \
     bbernhard/signal-cli-rest-api
   ```

2. **Register or link your Signal number**:

   - For a new number (register with SMS verification):
     ```sh
     curl -X POST 'http://localhost:8080/v1/register/+15551234567'
     curl -X POST 'http://localhost:8080/v1/verify/+15551234567' \
       -H 'Content-Type: application/json' \
       -d '{"code": "123-456"}'  # code received via SMS
     ```
   - For an existing Signal account (link as a secondary device):
     Open `http://localhost:8080/v1/qrcodelink?device_name=sorobeacon` in a
     browser, then on your phone: **Signal Settings → Linked devices → Link new device**
     and scan the QR code.

3. **Test the bridge**:

   ```sh
   curl -X POST 'http://localhost:8080/v2/send' \
     -H 'Content-Type: application/json' \
     -d '{"message": "Test from SoroBeacon!", "number": "+15551234567", "recipients": ["+15559876543"]}'
   ```

4. **Create the channel**:

   ```sh
   curl -s -X POST localhost:8080/api/v1/channels -d '{
     "name": "ops-signal",
     "type": "signal",
     "config": {
       "api_url": "http://signal-api:8080",
       "number": "+15551234567",
       "recipients": ["+15559876543", "+15551112222"]
     }
   }'
   ```

5. **Verify**:

   ```sh
   curl -s -X POST localhost:8080/api/v1/channels/1/test
   ```

## Config

| Key         | Required | Description |
|-------------|----------|-------------|
| `api_url`   | yes      | Base URL of the signal-cli-rest-api instance (e.g. `http://signal-api:8080`). Trailing slashes are stripped automatically. Plain `http://` is permitted because the bridge typically runs on a private network. |
| `number`    | yes      | The registered Signal number (sender) in international format, e.g. `+15551234567`. |
| `recipients`| yes      | Non-empty array of recipient numbers or group IDs. Numbers use international format (`+15559876543`); group IDs are the base64 IDs returned by signal-cli (e.g. `group.123...`). |
| `template`  | no       | Go `text/template` overriding the message. See [Message templates](templates.md). |

## Message format

Plain-text summary rendered from the shared alert template (override it with
[`template`](templates.md)):

```
🔔 SoroBeacon alert: Token treasury
Rule: value_threshold (#3)
Contract: CA7QYNF7...UWDA
Event: transfer
Ledger: 3721765
Tx: 4f2a...
Event ID: 0000015985...-0000000001
At: 2026-07-21 09:41:32 UTC
```

The JSON payload posted to `POST {api_url}/v2/send` is:

```json
{
  "message": "<rendered message>",
  "number": "+15551234567",
  "recipients": ["+15559876543", "+15551112222"]
}
```

## Error handling

- Non-2xx responses return an error containing the HTTP status code and a
  truncated response body. Recipient numbers are never included in error
  messages.
- Network failures are wrapped and surfaced as delivery errors.
- The channel config (which contains the sender number and recipient list) is
  never logged or returned in API responses.

## Notes

- The sender number must be registered/linked on the signal-cli-rest-api
  instance before creating the channel.
- Group IDs can be obtained via the signal-cli-rest-api `/v1/groups` endpoint
  or by sending a message to the group and inspecting the received envelope.
- For high availability, run multiple signal-cli-rest-api replicas behind a
  load balancer and point `api_url` at the balancer.