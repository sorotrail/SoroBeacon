## What this does

<!-- The behavior change, and why. Link the issue it closes if there is one. -->

## How you tested it

<!-- What you ran, and what you saw. Table-driven unit tests for rule types
     and channels; integration tests (TEST_DATABASE_URL) for store changes. -->

## Checklist

- [ ] `go build ./...`, `go test ./...` and `golangci-lint run` all pass
- [ ] Tests cover the change (or this is why they don't)
- [ ] No secrets (channel config, tokens, webhook URLs) end up in logs, error
      messages, API responses, or delivery `response_snippet`s
- [ ] New extension points (rule type, channel, event source) have a doc
      comment telling the next contributor how to use them
