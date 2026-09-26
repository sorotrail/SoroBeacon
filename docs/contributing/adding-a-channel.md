# Adding a Notification Channel: A Worked Walkthrough

This walkthrough takes you step-by-step through adding a new notification channel to SoroBeacon. We will use Discord (`internal/notify/discord.go`) as our model.

Adding a channel touches seven distinct areas: the `Notifier` interface, factory registration, configuration parsing and validation, secret redaction, the shared HTTP helper, unit tests, and the user documentation page.

---

## 1. The `Notifier` interface and what `Send` must guarantee

Every notification channel implements the `Notifier` interface defined in `internal/notify/notify.go`:

```go
type Notifier interface {
    Send(ctx context.Context, a Alert) error
}
```

### What `Send` must guarantee

- **Context respect:** Honor the passed `context.Context` (e.g. passing it down to `http.NewRequestWithContext`) so timeouts and cancellations abort outbound requests cleanly.
- **Error classification:** Return an error for any delivery failure (such as HTTP non-2xx responses, network timeouts, or protocol errors). Successful deliveries return `nil`.
- **No secrets in errors:** Never include credentials, webhook URLs, or tokens in the returned error. The dispatcher records returned errors in delivery attempt `response_snippet` columns and log lines.

---

## 2. Registration in the factory

Channels are created dynamically via a factory pattern. You must register your channel constructor in `DefaultFactory` inside `internal/notify/notify.go`:

```go
f.Register("discord", NewDiscord)
```

The string key (`"discord"`) determines the type string stored in the database and accepted by the HTTP API and dashboard when users configure channels.

---

## 3. Config parsing and validation

When a user creates or updates a channel, the JSON configuration blob is passed to your constructor function as `json.RawMessage`.

Define a private config struct with appropriate JSON struct tags, unmarshal the raw message, and validate all required fields. Field-level validation errors returned here reach the user as HTTP `400 Bad Request` messages.

```go
type discordConfig struct {
    WebhookURL string `json:"webhook_url"`
    Template   string `json:"template,omitempty"`
}

func NewDiscord(config json.RawMessage) (Notifier, error) {
    var cfg discordConfig
    if err := json.Unmarshal(config, &cfg); err != nil {
        return nil, fmt.Errorf("discord: invalid config: %w", err)
    }
    if cfg.WebhookURL == "" {
        return nil, fmt.Errorf("discord: webhook_url is required")
    }
    tpl, err := parseChannelTemplate(cfg.Template)
    if err != nil {
        return nil, fmt.Errorf("discord: %w", err)
    }
    return &Discord{cfg: cfg, tpl: tpl}, nil
}
```

---

## 4. Secret handling and encrypted config columns

### The Golden Rule

> **Never log, return, or echo channel `config` contents.**

Channel configs hold webhook URLs, bot tokens, API routing keys, and SMTP credentials. These secrets are encrypted at rest in the database via the store layer (`internal/store/crypto.go`), but once decrypted in memory, they must be strictly protected:

- **Never** include a webhook URL or token in error messages, log lines, or delivery response snippets.
- **Always** rely on `redactURLError` in `internal/notify/http.go` when handling outbound HTTP errors so that request URLs containing embedded credentials or tokens are stripped before the error is returned or logged:

```go
res, err := httpClient.Do(req)
if err != nil {
    return fmt.Errorf("%s: %w", strings.ToLower(method), redactURLError(err))
}
```

---

## 5. Using the shared HTTP helper

All HTTP-backed channels must use the shared helpers in `internal/notify/http.go` (`postJSON`, `requestJSON`, and `httpClient`) rather than instantiating a custom `http.Client`.

### Why?

- **Enforced timeouts:** The shared `httpClient` has a strict 15-second timeout preventing hung connections from blocking dispatchers.
- **Automatic redaction:** `requestJSON` automatically passes errors through `redactURLError`.
- **Response truncation:** Non-2xx responses automatically truncate response bodies to 300 bytes for safe storage in delivery attempt snippets.

```go
if err := postJSON(ctx, d.cfg.WebhookURL, body, nil); err != nil {
    return fmt.Errorf("discord: %w", err)
}
```

---

## 6. What tests should cover

Every channel must ship with a unit test suite (modelled on `internal/notify/discord_test.go` or `webhook_test.go`) covering:

1. **Constructor validation failures:** Passing empty or malformed JSON configurations returns expected validation errors.
2. **Successful delivery:** Mocking an HTTP server (`httptest.NewServer`) and confirming that `Send` successfully transmits the expected JSON payload and returns `nil` on 2xx responses.
3. **Failure outcomes:** Verifying that non-2xx status codes (e.g., `400`, `500`) are correctly caught and returned as errors.

---

## 7. User documentation under `docs/channels/`

Every new channel gets a dedicated markdown setup guide under `docs/channels/<name>.md` and must be linked in `docs/SUMMARY.md` under the **Channel reference** section. The guide should cover:

- Setup steps on the target service.
- API curl example for creating the channel.
- Config table detailing required and optional parameters.
- Example message or payload format.

---

## Contributor Checklist

- [ ] Implement `notify.Notifier` in `internal/notify/<channel>.go`.
- [ ] Register your channel constructor in `DefaultFactory` within `internal/notify/notify.go`.
- [ ] Ensure all config parsing validates required fields and returns clean errors.
- [ ] Ensure webhook URLs, tokens, and secrets are never logged, returned, or exposed in error messages or snippets (using `redactURLError`).
- [ ] Use the shared `postJSON` / `requestJSON` helpers from `internal/notify/http.go`.
- [ ] Write unit tests covering constructor errors and `Send` outcomes (`internal/notify/<channel>_test.go`).
- [ ] Create `docs/channels/<channel>.md` and link it in `docs/SUMMARY.md`.
- [ ] Run the test and verification suite successfully:
  ```sh
  go build ./...
  go vet ./...
  go test ./...
  golangci-lint run
  ```
