package notify

import (
	"encoding/json"
	"strings"
	"testing"

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
