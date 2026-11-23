package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/secrets"
)

func TestFactoryResolvesSecretReferences(t *testing.T) {
	env := &secrets.EnvProvider{Getenv: func(k string) string {
		if k == "SLACK_WEBHOOK_URL" {
			return "https://hooks.example/resolved"
		}
		return ""
	}}
	resolver := secrets.NewResolver(env)
	var captured slackConfig
	f := DefaultFactory().WithSecrets(resolver)
	f.Register("capture", func(cfg json.RawMessage) (Notifier, error) {
		return nil, json.Unmarshal(cfg, &captured)
	})

	if _, err := f.New("capture", json.RawMessage(`{"webhook_url":"${secret:env:SLACK_WEBHOOK_URL}"}`)); err != nil {
		t.Fatalf("New: %v", err)
	}
	if captured.WebhookURL != "https://hooks.example/resolved" {
		t.Fatalf("resolved webhook_url = %q", captured.WebhookURL)
	}
}

func TestFactoryWithoutResolverTreatsReferencesAsLiterals(t *testing.T) {
	var captured slackConfig
	f := DefaultFactory()
	f.Register("capture", func(cfg json.RawMessage) (Notifier, error) {
		return nil, json.Unmarshal(cfg, &captured)
	})
	if _, err := f.New("capture", json.RawMessage(`{"webhook_url":"${secret:env:SLACK_WEBHOOK_URL}"}`)); err != nil {
		t.Fatalf("New: %v", err)
	}
	if captured.WebhookURL != "${secret:env:SLACK_WEBHOOK_URL}" {
		t.Fatalf("reference was not passed through as a literal: %q", captured.WebhookURL)
	}
}

func TestFactoryResolutionFailureNamesReference(t *testing.T) {
	resolver := secrets.NewResolver(secrets.NewEnvProvider())
	f := DefaultFactory().WithSecrets(resolver)
	_, err := f.New(TypeSlack, json.RawMessage(`{"webhook_url":"${secret:env:MISSING_WEBHOOK}"}`))
	if err == nil {
		t.Fatal("expected a resolution error")
	}
	if !strings.Contains(err.Error(), "${secret:env:MISSING_WEBHOOK}") {
		t.Fatalf("error %q does not name the reference", err)
	}
}

// recordingSecretProvider is a settable in-memory provider with a fetch
// counter. The counter is what proves whether a delivery re-resolved a
// reference or was served from the resolver's cache.
type recordingSecretProvider struct {
	mu      sync.Mutex
	values  map[string]string
	fetches map[string]int
}

func newRecordingSecretProvider(values map[string]string) *recordingSecretProvider {
	return &recordingSecretProvider{values: values, fetches: map[string]int{}}
}

func (p *recordingSecretProvider) Scheme() string { return "recording" }

func (p *recordingSecretProvider) Fetch(_ context.Context, path, _ string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fetches[path]++
	v, ok := p.values[path]
	if !ok {
		return "", fmt.Errorf("no secret at %s", path)
	}
	return v, nil
}

func (p *recordingSecretProvider) set(path, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values[path] = value
}

func (p *recordingSecretProvider) fetchCount(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetches[path]
}

// captureWebhookFactory returns a factory whose "capture-secret" channel
// records the resolved webhook_url its constructor received.
func captureWebhookFactory(resolver *secrets.Resolver) (*Factory, func() string) {
	var captured string
	f := DefaultFactory().WithSecrets(resolver)
	f.Register("capture-secret", func(cfg json.RawMessage) (Notifier, error) {
		var c struct {
			WebhookURL string `json:"webhook_url"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, err
		}
		captured = c.WebhookURL
		return nil, nil
	})
	return f, func() string { return captured }
}

func secretConfig(path string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"webhook_url":"${secret:recording:%s}"}`, path))
}

// TestInvalidateSecretCachePicksUpRotation is the behaviour the function
// exists for: without invalidation the resolver keeps serving the cached
// credential until the TTL expires; after it, the next delivery sees the new
// value. Both halves are asserted, because the stale half is the bug.
func TestInvalidateSecretCachePicksUpRotation(t *testing.T) {
	provider := newRecordingSecretProvider(map[string]string{"slack": "old-value"})
	resolver := secrets.NewResolver(provider).WithTTL(time.Hour)
	factory, current := captureWebhookFactory(resolver)

	build := func() string {
		t.Helper()
		if _, err := factory.New("capture-secret", secretConfig("slack")); err != nil {
			t.Fatalf("New: %v", err)
		}
		return current()
	}

	if got := build(); got != "old-value" {
		t.Fatalf("first delivery = %q, want the initial secret", got)
	}
	if got := build(); got != "old-value" {
		t.Fatalf("cached delivery = %q, want the initial secret", got)
	}
	if n := provider.fetchCount("slack"); n != 1 {
		t.Fatalf("provider fetched %d times, want 1 (second delivery must hit the cache)", n)
	}

	provider.set("slack", "new-value")
	if got := build(); got != "old-value" {
		t.Fatalf("before invalidation = %q, want the stale value (TTL has not expired)", got)
	}

	factory.InvalidateSecretCache()
	if got := build(); got != "new-value" {
		t.Fatalf("after invalidation = %q, want the rotated value", got)
	}
	if n := provider.fetchCount("slack"); n != 2 {
		t.Fatalf("provider fetched %d times, want 2 (invalidation must force one re-resolve)", n)
	}
}

// TestInvalidateSecretCacheWithoutResolverIsNoop pins the nil guard: a
// deployment without external secrets must not panic when the API invalidates
// after a channel edit.
func TestInvalidateSecretCacheWithoutResolverIsNoop(t *testing.T) {
	factory := DefaultFactory()
	factory.InvalidateSecretCache()
}

// TestInvalidateSecretCacheWithEmptyCacheIsNoop pins that clearing an empty
// cache is harmless and does not resolve or fetch anything.
func TestInvalidateSecretCacheWithEmptyCacheIsNoop(t *testing.T) {
	provider := newRecordingSecretProvider(map[string]string{})
	factory := DefaultFactory().WithSecrets(secrets.NewResolver(provider))

	factory.InvalidateSecretCache()

	if n := provider.fetchCount("anything"); n != 0 {
		t.Fatalf("invalidation fetched %d secrets, want 0", n)
	}
}

// TestInvalidateSecretCacheIsProcessWide documents the scope of invalidation.
// The resolver holds one cache for the process, not one per channel, so
// clearing it re-resolves every referenced secret on the next delivery. That
// is deliberate — the cache key has no channel id and a rotated secret is not
// attributable to one channel — and docs/channels/secrets.md says so.
func TestInvalidateSecretCacheIsProcessWide(t *testing.T) {
	provider := newRecordingSecretProvider(map[string]string{
		"slack":   "slack-value",
		"discord": "discord-value",
	})
	factory, current := captureWebhookFactory(secrets.NewResolver(provider).WithTTL(time.Hour))

	build := func(path string) string {
		t.Helper()
		if _, err := factory.New("capture-secret", secretConfig(path)); err != nil {
			t.Fatalf("New: %v", err)
		}
		return current()
	}

	build("slack")
	build("discord")
	factory.InvalidateSecretCache()
	build("slack")
	build("discord")

	for _, path := range []string{"slack", "discord"} {
		if n := provider.fetchCount(path); n != 2 {
			t.Fatalf("%s fetched %d times, want 2: invalidation is process-wide", path, n)
		}
	}
}

// TestInvalidateSecretCacheIsSafeConcurrentlyWithDeliveries is the race-
// detector case: invalidation takes the resolver's lock while deliveries read
// and write the cache. Run with -race; a synchronization mistake here would
// corrupt the map or race the TTL clock.
func TestInvalidateSecretCacheIsSafeConcurrentlyWithDeliveries(t *testing.T) {
	provider := newRecordingSecretProvider(map[string]string{"slack": "value"})
	factory := DefaultFactory().WithSecrets(secrets.NewResolver(provider).WithTTL(time.Hour))
	factory.Register("concurrent-secret", func(json.RawMessage) (Notifier, error) {
		return nil, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, err := factory.New("concurrent-secret", secretConfig("slack")); err != nil {
					t.Errorf("New: %v", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			factory.InvalidateSecretCache()
		}
	}()
	wg.Wait()

	if n := provider.fetchCount("slack"); n == 0 {
		t.Fatal("the concurrent deliveries never resolved the secret")
	}
}
