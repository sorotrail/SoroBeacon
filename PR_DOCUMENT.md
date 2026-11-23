# PR: Add edge cases to NewPruner tests

## Summary
Added table-driven unit tests for the `NewPruner` function in `internal/archive/archive.go`.

## Changes

### Enhanced file: `internal/archive/archive_test.go`
- Added `TestNewPrunerEdgeCases` with table-driven tests covering:
  - **Error path**: nil backend returns an error `"archive: nil backend"`
  - **Happy path**: valid backend (Dir archiver) succeeds

### Test Cases
1. `nil_backend_returns_error` - verifies that passing a nil backend to `NewPruner` returns an error
2. `valid_backend_succeeds` - verifies that a valid backend (directory-based archiver) allows `NewPruner` to succeed

## Test Results
Both test cases pass:
- `nil_backend_returns_error` ✓
- `valid_backend_succeeds` ✓

## Checklist
- [x] Edge case tests added for `NewPruner` function
- [x] Table-driven test structure used (consistent with project style)
- [x] Nil backend error case covered
- [x] Valid backend happy path covered
- [x] All existing tests continue to pass