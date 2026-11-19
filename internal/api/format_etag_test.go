package api

import (
	"regexp"
	"testing"
)

// strongETag is a quoted, 32-hex-digit strong entity tag (128 bits of SHA-256).
var strongETag = regexp.MustCompile(`^"[0-9a-f]{32}"$`)

func TestFormatETag(t *testing.T) {
	base := FormatETag("acme", "/api/v1/monitors", "limit=10", "change-1")

	if !strongETag.MatchString(base) {
		t.Fatalf("FormatETag() = %s, want a quoted 32-hex-digit strong tag", base)
	}
	if again := FormatETag("acme", "/api/v1/monitors", "limit=10", "change-1"); again != base {
		t.Fatalf("FormatETag is not deterministic: %s then %s", base, again)
	}
	// ETagMatches has to accept the tag FormatETag produced, or conditional
	// requests could never hit.
	if !ETagMatches(base, base) {
		t.Fatalf("ETagMatches(%s, %s) = false, want true", base, base)
	}

	// Every input changes the answer, so each must change the tag.
	tests := []struct {
		name                       string
		ws, path, query, changeKey string
	}{
		{name: "another workspace", ws: "other", path: "/api/v1/monitors", query: "limit=10", changeKey: "change-1"},
		{name: "another path", ws: "acme", path: "/api/v1/rules", query: "limit=10", changeKey: "change-1"},
		{name: "another query", ws: "acme", path: "/api/v1/monitors", query: "limit=20", changeKey: "change-1"},
		{name: "another change key", ws: "acme", path: "/api/v1/monitors", query: "limit=10", changeKey: "change-2"},
		{name: "all empty", ws: "", path: "", query: "", changeKey: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatETag(tc.ws, tc.path, tc.query, tc.changeKey)
			if !strongETag.MatchString(got) {
				t.Fatalf("FormatETag() = %s, want a quoted 32-hex-digit strong tag", got)
			}
			if got == base {
				t.Fatalf("FormatETag(%q, %q, %q, %q) = %s, same as the base tag", tc.ws, tc.path, tc.query, tc.changeKey, got)
			}
		})
	}
}

// The fields are joined with "|", so moving text across a field boundary must
// still change the tag rather than collide.
func TestFormatETagFieldBoundariesDoNotCollide(t *testing.T) {
	a := FormatETag("acme", "/x", "a=1", "k")
	b := FormatETag("acme", "/x", "a=1|k", "")
	if a == b {
		t.Fatalf("FormatETag collided across a field boundary: %s", a)
	}
}
