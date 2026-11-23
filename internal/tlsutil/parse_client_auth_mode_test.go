package tlsutil

import (
	"strings"
	"testing"
)

func TestParseClientAuthMode(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		hasClientCA bool
		want        ClientAuthMode
	}{
		{name: "empty without a client CA defaults to none", raw: "", want: ClientAuthNone},
		{name: "empty with a client CA defaults to require", raw: "", hasClientCA: true, want: ClientAuthRequire},
		{name: "whitespace only is treated as empty", raw: "   ", hasClientCA: true, want: ClientAuthRequire},
		{name: "none", raw: "none", hasClientCA: true, want: ClientAuthNone},
		{name: "request", raw: "request", want: ClientAuthRequest},
		{name: "require", raw: "require", want: ClientAuthRequire},
		{name: "case and surrounding space are normalised", raw: "  ReQuIrE ", want: ClientAuthRequire},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseClientAuthMode(tc.raw, tc.hasClientCA)
			if err != nil {
				t.Fatalf("ParseClientAuthMode(%q, %v) returned error: %v", tc.raw, tc.hasClientCA, err)
			}
			if got != tc.want {
				t.Fatalf("ParseClientAuthMode(%q, %v) = %q, want %q", tc.raw, tc.hasClientCA, got, tc.want)
			}
		})
	}
}

func TestParseClientAuthModeRejectsUnknownModes(t *testing.T) {
	for _, raw := range []string{"optional", "verify", "required", "none,require"} {
		t.Run(raw, func(t *testing.T) {
			got, err := ParseClientAuthMode(raw, true)
			if err == nil {
				t.Fatalf("ParseClientAuthMode(%q) = %q, want an error", raw, got)
			}
			if got != "" {
				t.Fatalf("ParseClientAuthMode(%q) returned mode %q alongside an error", raw, got)
			}
			if !strings.Contains(err.Error(), raw) {
				t.Fatalf("error %q should quote the rejected value %q", err, raw)
			}
		})
	}
}
