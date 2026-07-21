# Email (SMTP)

Sends each alert as a plain-text email over SMTP (STARTTLS when the server offers it).

## Setup

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "alerts-mail",
  "type": "email",
  "config": {
    "host": "smtp.example.com",
    "port": 587,
    "username": "beacon",
    "password": "app-password",
    "from": "beacon@example.com",
    "to": ["ops@example.com", "oncall@example.com"]
  }
}'

curl -s -X POST localhost:8080/api/v1/channels/4/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `host` | yes | SMTP server hostname. |
| `port` | no | Defaults to `587`. |
| `username` | no | SMTP auth user; omit for unauthenticated relays. |
| `password` | no | SMTP auth password. Treated as a secret. |
| `from` | yes | Sender address. |
| `to` | yes | List of recipient addresses. |

The subject line is `SoroBeacon alert: <monitor name>`; the body is the standard alert summary.

{% hint style="info" %}
For Gmail/Google Workspace use an **app password**, not the account password.
{% endhint %}
