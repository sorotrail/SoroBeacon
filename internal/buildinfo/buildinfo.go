// Package buildinfo carries version information injected at build time via
// -ldflags. Values are "dev" when unset, i.e. for go run / go build without
// flags. Release builds set all three:
//
//	go build -ldflags "\
//	  -X github.com/sorotrail/sorobeacon/internal/buildinfo.Version=v1.2.3 \
//	  -X github.com/sorotrail/sorobeacon/internal/buildinfo.Commit=$(git rev-parse --short HEAD) \
//	  -X github.com/sorotrail/sorobeacon/internal/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package buildinfo

// Version is the release tag, e.g. v1.2.3.
var Version = "dev"

// Commit is the short git hash the binary was built from.
var Commit = "none"

// Date is the UTC build timestamp, RFC 3339.
var Date = "unknown"

// Info is the JSON shape served by GET /api/v1/version.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"build_date"`
}
