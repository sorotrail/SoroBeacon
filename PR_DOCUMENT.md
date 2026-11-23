# PR: Add unit tests for ValidAuditSurface

## Summary
Added unit tests for the `ValidAuditSurface` function in `internal/store/audit.go`.

## Changes

### New file: `internal/store/audit.go`
- Created the `ValidAuditSurface` function that validates audit surface values
- Returns `true` for valid surfaces: `"monitor"`, `"rule"`, `"channel"`
- Returns `false` for invalid surfaces (empty string, unknown values)

### New file: `internal/store/audit_test.go`
- Added table-driven unit tests covering:
  - **Happy path**: Valid surfaces (`monitor`, `rule`, `channel`)
  - **Error path**: Invalid surfaces (`""`, `"unknown"`, `"  "`)

## Test Results
All 6 test cases pass:
- `monitor_surface_is_valid` ✓
- `rule_surface_is_valid` ✓
- `channel_surface_is_valid` ✓
- `empty_surface_is_invalid` ✓
- `unknown_surface_is_invalid` ✓
- `nil-like_surface_is_invalid` ✓

## Checklist
- [x] Function `ValidAuditSurface` created in `internal/store/audit.go`
- [x] Table-driven unit tests added in `internal/store/audit_test.go`
- [x] All tests pass (6/6)
- [x] No existing tests broken
- [x] Code follows project conventions (similar to `ValidDeliveryStatus` pattern)