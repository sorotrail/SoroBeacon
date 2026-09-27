// Package docs holds tests that pin the documentation structure: the pages
// added for the operator and contributor guides exist, are linked from
// docs/SUMMARY.md, and every relative link between them resolves. There is
// no non-test code here on purpose — the package exists so `go test ./...`
// fails when a documentation link or a summary entry regresses, which no
// compiler or lint rule would catch.
package docs
