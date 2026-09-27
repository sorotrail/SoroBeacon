package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProvider is the small third-provider proof: Scheme + Fetch is all a
// provider needs. It records calls so tests can assert on caching.
type fakeProvider struct {
	mu     sync.Mutex
	calls  int
	values map[string]string
	err    error
}

func (f *fakeProvider) Scheme() string { return "fake" }

func (f *fakeProvider) Fetch(_ context.Context, path, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.values[path+"#"+key], nil
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestParseReference(t *testing.T) {
	tests := []struct {
		in   string
		want Reference
		ok   bool
	}{
		{"${secret:vault:kv/data/sorobeacon#slack_token}", Reference{"vault", "kv/data/sorobeacon", "slack_token"}, true},
		{"${secret:env:SLACK_WEBHOOK_URL}", Reference{"env", "SLACK_WEBHOOK_URL", ""}, true},
		{"plain-literal", Reference{}, false},
		{"prefix ${secret:env:X} suffix", Reference{}, false},
		{"${secret:env:}", Reference{}, false},
		{"${secret::path}", Reference{}, false},
		{"${secret:ENV:path}", Reference{}, false},
		{"${secret:env:a b}", Reference{}, false},
		{"${secret:env:x#a#b}", Reference{}, false},
		{"", Reference{}, false},
	}
	for _, tt := range tests {
		got, ok := ParseReference(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("ParseReference(%q) = (%+v, %v), want (%+v, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestResolveStringLiteralsPassThrough(t *testing.T) {
	r := NewResolver(&fakeProvider{values: map[string]string{}})
	for _, s := range []string{"https://hooks.slack.com/services/abc", "", "${secret:}", "${secret:env:X", "not-a-ref"} {
		got, err := r.ResolveString(context.Background(), s)
		if err != nil {
			t.Fatalf("ResolveString(%q) unexpected error: %v", s, err)
		}
		if got != s {
			t.Errorf("ResolveString(%q) = %q; literals must pass through unchanged", s, got)
		}
	}
}

func TestResolveSuccessAndCache(t *testing.T) {
	fp := &fakeProvider{values: map[string]string{"kv/data/sorobeacon#slack_token": "s3cr3t"}}
	r := NewResolver(fp)
	ctx := context.Background()

	got, err := r.ResolveString(ctx, "${secret:fake:kv/data/sorobeacon#slack_token}")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("resolved %q, want s3cr3t", got)
	}
	// A second resolution must be served from the cache.
	if _, err := r.ResolveString(ctx, "${secret:fake:kv/data/sorobeacon#slack_token}"); err != nil {
		t.Fatalf("resolve (cached): %v", err)
	}
	if n := fp.callCount(); n != 1 {
		t.Fatalf("provider called %d times, want 1 (cached)", n)
	}

	// Expiring the entry causes a refetch.
	r.now = func() time.Time { return time.Now().Add(2 * DefaultTTL) }
	if _, err := r.ResolveString(ctx, "${secret:fake:kv/data/sorobeacon#slack_token}"); err != nil {
		t.Fatalf("resolve (expired): %v", err)
	}
	if n := fp.callCount(); n != 2 {
		t.Fatalf("provider called %d times after TTL, want 2", n)
	}

	// Invalidate drops the cache so a rotated value is picked up at once.
	fp.values["kv/data/sorobeacon#slack_token"] = "rotated"
	r.now = time.Now
	r.Invalidate()
	got, err = r.ResolveString(ctx, "${secret:fake:kv/data/sorobeacon#slack_token}")
	if err != nil {
		t.Fatalf("resolve (invalidated): %v", err)
	}
	if got != "rotated" {
		t.Fatalf("resolved %q after invalidate, want rotated", got)
	}
}

func TestResolveFailureNamesReferenceNotValue(t *testing.T) {
	fp := &fakeProvider{
		values: map[string]string{"path#key": "super-secret-value"},
		err:    errors.New("connection refused"),
	}
	r := NewResolver(fp)
	_, err := r.ResolveString(context.Background(), "${secret:fake:path#key}")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "${secret:fake:path#key}") {
		t.Errorf("error %q does not name the reference", err)
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Errorf("error %q leaked the resolved value", err)
	}

	// An unknown provider is a clear error too.
	if _, err := r.ResolveString(context.Background(), "${secret:nope:x}"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("unknown provider error = %v, want one naming the provider", err)
	}
}

func TestResolveConfig(t *testing.T) {
	fp := &fakeProvider{values: map[string]string{"kv/data/sorobeacon#slack_token": "https://hooks.example/abc"}}
	r := NewResolver(fp)
	raw := json.RawMessage(`{
		"webhook_url": "${secret:fake:kv/data/sorobeacon#slack_token}",
		"channel": "#ops",
		"nested": {"token": "${secret:fake:kv/data/sorobeacon#slack_token}"},
		"port": 587,
		"huge": 100000000000000000000
	}`)
	out, err := r.ResolveConfig(context.Background(), raw)
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	// The original document is untouched: it still holds the reference.
	if !strings.Contains(string(raw), "${secret:fake:") {
		t.Fatalf("input config was mutated: %s", raw)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("resolved config is not valid JSON: %v", err)
	}
	if got["webhook_url"] != "https://hooks.example/abc" {
		t.Errorf("webhook_url = %v", got["webhook_url"])
	}
	if got["channel"] != "#ops" {
		t.Errorf("literal channel was altered: %v", got["channel"])
	}
	nested, _ := got["nested"].(map[string]any)
	if nested["token"] != "https://hooks.example/abc" {
		t.Errorf("nested token = %v", nested["token"])
	}
	// Numbers must survive without becoming floats.
	if !strings.Contains(string(out), "100000000000000000000") {
		t.Errorf("large integer was mangled: %s", out)
	}
}

func TestResolveConfigInvalidJSONPassesThrough(t *testing.T) {
	r := NewResolver(&fakeProvider{values: map[string]string{}})
	raw := json.RawMessage(`{not json`)
	out, err := r.ResolveConfig(context.Background(), raw)
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	if string(out) != string(raw) {
		t.Errorf("invalid JSON was changed: %q -> %q", raw, out)
	}
}

func TestEnvProvider(t *testing.T) {
	p := &EnvProvider{Getenv: func(k string) string {
		if k == "SLACK_WEBHOOK_URL" {
			return "https://hooks.example/env"
		}
		return ""
	}}
	r := NewResolver(p)
	got, err := r.ResolveString(context.Background(), "${secret:env:SLACK_WEBHOOK_URL}")
	if err != nil || got != "https://hooks.example/env" {
		t.Fatalf("env resolve = (%q, %v)", got, err)
	}
	// A key overrides the path as the variable name.
	got, err = r.ResolveString(context.Background(), "${secret:env:namespace#SLACK_WEBHOOK_URL}")
	if err != nil || got != "https://hooks.example/env" {
		t.Fatalf("env resolve with key = (%q, %v)", got, err)
	}
	if _, err := r.ResolveString(context.Background(), "${secret:env:MISSING}"); err == nil {
		t.Fatal("expected an error for an unset variable")
	}
}
