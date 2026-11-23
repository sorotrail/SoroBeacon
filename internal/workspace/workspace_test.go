package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidAcceptsOnlyTheDocumentedGrammar(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"default", true},
		{"acme", true},
		{"acme-eu", true},
		{"acme_eu2", true},
		{"2start", true},
		{"a", true},
		{repeat("b", idMaxLen), true},
		{"", false},
		{repeat("c", idMaxLen+1), false},
		{"Acme", false},
		{"acme eu", false},
		{"acme.eu", false},
		{"acme/eu", false},
		{"-acme", false},
		{"_acme", false},
		{"acme;drop", false},
		{"ünïcode", false},
		{"acme\x00", false},
	}
	for _, tc := range tests {
		if got := Valid(tc.id); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestParseMapsEmptyToDefault(t *testing.T) {
	// A deployment that never configured tenancy must keep working, and the
	// only way it can express that is an absent value.
	id, err := Parse("")
	if err != nil || id != Default {
		t.Fatalf(`Parse("") = (%q, %v), want (%q, nil)`, id, err, Default)
	}
	if !Valid(string(Default)) {
		t.Fatal("Default must be a valid id, or every scoped query is scoped to garbage")
	}
}

func TestParseRejectsInvalidIDsWithoutEchoingThem(t *testing.T) {
	for _, bad := range []string{"Acme", "acme eu", "acme;drop table", "-", repeat("z", idMaxLen+1)} {
		id, err := Parse(bad)
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalidID", bad, err)
		}
		if id != "" {
			t.Errorf("Parse(%q) = %q, want the empty id alongside the error", bad, id)
		}
		// A rejected value is often attacker-supplied, and this error reaches
		// log lines; the message must stay generic.
		if strings.Contains(err.Error(), bad) {
			t.Errorf("Parse error %q echoes the rejected value %q", err, bad)
		}
	}
}

func TestWithAndFromRoundTrip(t *testing.T) {
	if _, ok := From(context.Background()); ok {
		t.Fatal("From on a bare context must report no workspace")
	}
	ctx := With(context.Background(), ID("acme"))
	id, ok := From(ctx)
	if !ok || id != ID("acme") {
		t.Fatalf("From = (%q, %v), want (acme, true)", id, ok)
	}
	// Nesting must overwrite rather than merge, so the innermost scope wins.
	inner := With(ctx, ID("beta"))
	if id, _ := From(inner); id != ID("beta") {
		t.Fatalf("From(nested) = %q, want beta", id)
	}
	if id, _ := From(ctx); id != ID("acme") {
		t.Fatalf("parent context changed to %q", id)
	}
}

func TestSystemScopeCarriesNoWorkspace(t *testing.T) {
	ctx := WithSystem(context.Background())
	if !System(ctx) {
		t.Fatal("System must report the scope it was given")
	}
	if _, ok := From(ctx); ok {
		t.Fatal("a system context must not read back as one workspace, or cross-tenant callers would scope themselves")
	}
	if System(context.Background()) {
		t.Fatal("a bare context is not the system scope: defaulting must not look like an audit sweep")
	}
	// The two keys are independent, so nesting them leaves both set. That is
	// the store's contract, not a leak: tenantWorkspace asks System first, so a
	// context escalated to the system scope is treated as cross-tenant by every
	// method that can serve one, and by no method that cannot.
	if id, ok := From(WithSystem(With(context.Background(), ID("acme")))); !ok || id != ID("acme") {
		t.Fatalf("From after WithSystem = (%q, %v), want the inner scope to stay readable for callers that check System", id, ok)
	}
}

func TestFromIgnoresForeignContextValues(t *testing.T) {
	// The key type is private, so the only supported writer is With. A package
	// that guesses a key shape must not be able to spoof a scope.
	type lookalike struct{}
	ctx := context.WithValue(context.Background(), lookalike{}, ID("acme"))
	if _, ok := From(ctx); ok {
		t.Fatal("From read a value it did not set")
	}
	// An int under the right key shape is a different bug but the same answer.
	if _, ok := From(context.WithValue(context.Background(), ctxKey{}, "not an ID")); ok {
		t.Fatal("From accepted a value of the wrong type")
	}
}

// repeat returns r concatenated n times, so the length bounds in the grammar
// tests above read as data instead of as calls into the standard library.
func repeat(r string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(r)
	}
	return b.String()
}
