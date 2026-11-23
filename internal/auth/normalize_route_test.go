package auth

import "testing"

func TestNormalizeRoute(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "already normal", in: "/api/v1/monitors", want: "/api/v1/monitors"},
		{name: "missing leading slash", in: "api/v1/monitors", want: "/api/v1/monitors"},
		{name: "trailing slash removed", in: "/api/v1/monitors/", want: "/api/v1/monitors"},
		{name: "several trailing slashes removed", in: "/api/v1/monitors///", want: "/api/v1/monitors"},
		{name: "surrounding space trimmed", in: "  /healthz \t", want: "/healthz"},
		{name: "pattern segments kept", in: "monitors/{id}/", want: "/monitors/{id}"},
		{name: "empty is the root", in: "", want: "/"},
		{name: "whitespace only is the root", in: "   ", want: "/"},
		{name: "root stays root", in: "/", want: "/"},
		{name: "only slashes collapse to the root", in: "///", want: "/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeRoute(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeRoute(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := NormalizeRoute(got); again != got {
				t.Fatalf("NormalizeRoute is not idempotent: %q then %q", got, again)
			}
		})
	}
}
