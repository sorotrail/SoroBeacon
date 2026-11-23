// Package buildinfo carries version information injected at build time via
// -ldflags. Values are "dev" when unset, i.e. for go run / go build without
// flags. Release builds set all three deterministically from git metadata:
//
//	DATE=$(TZ=UTC git show -s --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD)
//	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "\
//	  -s -w -buildid= \
//	  -X github.com/sorotrail/sorobeacon/internal/buildinfo.Version=v1.2.3 \
//	  -X github.com/sorotrail/sorobeacon/internal/buildinfo.Commit=$(git rev-parse --short=12 HEAD) \
//	  -X github.com/sorotrail/sorobeacon/internal/buildinfo.Date=${DATE}" \
//	  ./cmd/sorobeacon
package buildinfo

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Version is the release tag, e.g. v1.2.3.
var Version = "dev"

// Commit is the short git hash the binary was built from.
var Commit = "none"

// Date is the UTC commit/build timestamp, RFC 3339.
var Date = "unknown"

// Info is the JSON shape served by GET /api/v1/version.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"build_date"`
}

// Current returns the stamped build metadata, normalizing Date to canonical
// UTC RFC 3339 when Date holds a valid RFC 3339 timestamp with a non-UTC
// offset or a decimal SOURCE_DATE_EPOCH unix timestamp.
func Current() Info {
	normalized := Date
	if d, err := NormalizeDate(Date); err == nil {
		normalized = d
	}
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    normalized,
	}
}

// NormalizeDate converts either an RFC 3339 timestamp (such as `git show -s
// --format=%cI HEAD`, which may carry a committer timezone offset) or a
// decimal Unix epoch string (`SOURCE_DATE_EPOCH`) into canonical UTC RFC 3339
// (`YYYY-MM-DDTHH:MM:SSZ`) so two builders in different timezones always stamp
// identical strings.
func NormalizeDate(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("empty build date")
	}
	if epoch, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return time.Unix(epoch, 0).UTC().Format(time.RFC3339), nil
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return "", fmt.Errorf("parse build date %q: %w", raw, err)
	}
	return parsed.UTC().Format(time.RFC3339), nil
}
