# PR: Add edge cases to NewPruner tests

## Summary
Added 2 new table-driven edge cases to `TestNewPrunerEdgeCases` in `internal/archive/archive_test.go`:
- `empty alerts is no-op` - verifies that empty alert batch is handled as a no-op
- `single alert boundary` - verifies single alert processing and deterministic key generation

## Changes
- `internal/archive/archive_test.go`: Added 2 new test cases to the existing table-driven test
- Added `context` and `time` imports
- Added test for empty alerts input (boundary condition)
- Added test for single alert (boundary value with deterministic key verification)

## Test Cases
1. **nil backend returns error** - Existing: verifies nil backend returns error
2. **valid backend succeeds** - Existing: verifies valid backend succeeds
3. **empty alerts is no-op** - New: verifies empty alerts slice succeeds as no-op
4. **single alert boundary** - New: verifies single alert processing and `BatchKey` deterministic key generation

## Verification
Run `go test -v ./internal/archive/...` to execute all 4 test cases (including 2 new ones).