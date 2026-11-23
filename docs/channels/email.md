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
| `subject_prefix` | no | Prepended to the subject verbatim when set (e.g. `"[PROD] "` — include the trailing space if you want one). Empty by default. |
| `template` | no | Go `text/template` overriding the message **body**. See [Message templates](templates.md). |
| `require_tls` | no | Refuse to deliver unless the session is encrypted. `false` by default — see [TLS](#tls). |
| `tls_skip_verify` | no | Do not verify the relay's certificate. `false` by default — see [TLS](#tls). |
| `connect_timeout_seconds` | no | Seconds allowed to establish the session (the TCP connect and the server's greeting). Defaults to `10`. |
| `timeout_seconds` | no | Seconds allowed for the whole delivery, greeting included. Defaults to `30`. |

The subject line is `<subject_prefix>SoroBeacon alert: <monitor name>`; without a `template` the body is the standard alert summary.

{% hint style="info" %}
For Gmail/Google Workspace use an **app password**, not the account password.
{% endhint %}

## TLS

STARTTLS is **opportunistic** in the SMTP protocol: the client upgrades when
the server advertises it and carries on in plaintext when it does not. That
is fine on a trusted network and wrong on an untrusted one — a relay that
stops advertising STARTTLS silently downgrades the session, and the alert
and the SMTP credentials cross the network in the clear.

* **`require_tls: true` makes the client refuse the downgrade.** When the
  server does not offer STARTTLS, the delivery fails with `the server does
  not offer STARTTLS but the channel requires TLS` and nothing is sent. The
  same happens when the handshake itself fails: the alert is never sent over
  a session that was not encrypted.
* The relay's certificate must be valid for the configured `host`, and it is
  verified against the system trust store. A relay with a private
  certificate needs either a CA the host trusts or `tls_skip_verify: true`.
* **`tls_skip_verify: true` turns certificate verification off.** The
  session is still encrypted, but the relay's identity is no longer checked,
  so anyone able to intercept the connection can present their own
  certificate. Use it only for a relay on a network you control, and prefer
  a trusted CA where you can.

Credentials are only ever offered to a server that can encrypt the session
(`net/smtp` refuses to authenticate over an unencrypted connection to
anything but localhost), and they never appear in an error message or a log
line: the channel's own values — the password, the username, the addresses —
are stripped from anything the server says before it is recorded as a
delivery failure. The login uses the `PLAIN` mechanism (RFC 4616), so the
relay has to offer `AUTH PLAIN`; a server that offers no authentication at
all is reported as a configuration mistake rather than delivered to
anonymously.

## Timeouts

`connect_timeout_seconds` bounds establishing the session and
`timeout_seconds` bounds the whole delivery. A server that accepts the
connection and then stops answering is cut off at the deadline instead of
holding the delivery goroutine, and a cancelled delivery stops at once.
Both are also clamped by the channel's own timeout (the `timeout_seconds`
field of the channel record), so the effective deadline is the shorter of
the two.

## Recipients

Every address in `to` gets its own `RCPT TO`, and one message is sent with
all of them in the `To` header. If the server refuses one of the addresses
the delivery is **reported as a failure** — the addresses the server did
accept still receive the alert, but the delivery is not silently counted as
a success.

An address that contains a line break is refused when the channel is
created: it would end the header it sits in and turn the rest of the value
into headers of its own. The same goes for `from` and `subject_prefix`.
The monitor name in the subject is sanitised rather than refused, because it
comes from the store rather than from the channel config.
