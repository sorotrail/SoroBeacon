## Summary

This PR implements end-to-end timeout and cancellation propagation for inbound API requests while ensuring that critical background operations (alert persistence, cursor advances, delivery records, and escalation state transitions) are explicitly detached to complete safely even if a caller disconnects.

## Changes

- Propagated request contexts through handlers, stores, and database query executions.
- Added explicit context detachment (`context.WithoutCancel` / background contexts) for asynchronous dispatcher, poller, and notification worker paths.
- Ensured database statement timeouts and connection-level cancel handling are correctly bound.
- Distinguished client disconnections and cancellations in metrics and logs from internal server faults to protect error budgets.

## Verification

- Ran `go build ./...` and `go vet ./...` successfully.
- Executed full test suite including SQLite and Postgres conformance tests.
- Verified `golangci-lint run` passes cleanly with no warnings or errors.
