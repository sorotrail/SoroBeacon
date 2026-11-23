# AWS SNS

Publishes alerts to an **Amazon SNS topic** using the AWS SDK for Go v2 with SigV4 request signing. Downstream subscribers (Lambda, SQS, email, SMS, etc.) receive the full alert as structured JSON.

## Setup

### Option 1: Static credentials (for testing or cross-account)

1. Create an IAM user with `sns:Publish` permission on the target topic.
2. Copy the Access Key ID and Secret Access Key.
3. Create the channel:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "alerts-sns",
  "type": "sns",
  "config": {
    "topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon",
    "region": "us-east-1",
    "access_key_id": "AKIA...",
    "secret_access_key": "..."
  }
}'
```

### Option 2: Default credential chain (recommended for production)

When `access_key_id` and `secret_access_key` are omitted, the AWS SDK's default credential chain is used. This works with:
- EC2/ECS instance roles
- ECS task roles
- EKS pod identities (IRSA)
- Environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`)
- Shared config/credentials files (`~/.aws/config`, `~/.aws/credentials`)
- AWS SSO

Create the channel without static credentials:

```sh
curl -s -X POST localhost:8080/api/v1/channels -d '{
  "name": "alerts-sns",
  "type": "sns",
  "config": {
    "topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon",
    "region": "us-east-1"
  }
}'
```

### IAM Policy

The identity (user or role) needs only `sns:Publish` on the topic:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "sns:Publish",
      "Resource": "arn:aws:sns:us-east-1:123456789012:sorobeacon"
    }
  ]
}
```

3. Verify:

```sh
curl -s -X POST localhost:8080/api/v1/channels/1/test
```

## Config

| Key | Required | Description |
| --- | --- | --- |
| `topic_arn` | yes | Full ARN of the SNS topic (e.g., `arn:aws:sns:us-east-1:123456789012:sorobeacon`). |
| `region` | yes | AWS region of the topic (e.g., `us-east-1`, `eu-west-1`). |
| `access_key_id` | no | Static access key. When omitted, the SDK's default credential chain is used. |
| `secret_access_key` | no | Static secret key. When omitted, the SDK's default credential chain is used. **Treated as a secret** — never logged or returned by the API. |

`region` is authoritative: construction checks only that `topic_arn` starts
with `arn:aws:sns:`, and does not compare the ARN's embedded region with the
`region` field. A mismatch is accepted when the channel is created and
surfaces as a provider-side error on the first publish, so keep the two
aligned. Static credentials are likewise used only when **both** fields are
present; supplying one alone leaves the default chain in charge.

## Payload

Each alert is published as a JSON message to the SNS topic:

```json
{
  "monitor_id": 9,
  "monitor_name": "Token treasury",
  "rule_id": 3,
  "rule_type": "event_emitted",
  "event_id": "0000015985348674617345-0000000001",
  "contract_id": "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
  "event_name": "transfer",
  "ledger": 3721765,
  "tx_hash": "4f2a...",
  "payload": {"amount": "1000000000"},
  "created_at": "2026-07-21T09:41:32Z",
  "severity": "warning"
}
```

All alert fields are included so downstream subscribers can parse and route without calling back to SoroBeacon.

Non-2xx responses (returned as SDK errors) count as failures and are retried with backoff; the error carries the status code but never the secret access key.

## Credential modes

| Mode | When to use | How it works |
| --- | --- | --- |
| **Static credentials** | Testing, cross-account, environments without IAM roles | Provide `access_key_id` and `secret_access_key` in config. |
| **Default credential chain** | Production on AWS (EC2, ECS, EKS, Lambda) | Omit static credentials; SDK resolves from instance role, env vars, config files, etc. |

Both modes use SigV4 signing automatically via the AWS SDK.

Construction does not probe for credentials. With static credentials omitted,
the channel is accepted even if no credential is currently available: the SDK
resolves and refreshes credentials per request, which is what allows an
instance role that appears after boot to work. A missing credential therefore
fails the first delivery, not channel creation.

## Security

- The `secret_access_key` is **treated as a secret** — never logged, never returned by the API, never included in error messages or delivery response snippets.
- When using static credentials, store them encrypted at rest by setting `CONFIG_ENCRYPTION_KEY`.
- Prefer the default credential chain in production to avoid managing long-lived keys.