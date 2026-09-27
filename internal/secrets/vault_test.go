package secrets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVaultProviderKVv2(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Vault-Token")
		if r.URL.Path != "/v1/kv/data/sorobeacon" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"slack_token":"https://hooks.example/vault","other":"x"},"metadata":{}}}`))
	}))
	defer srv.Close()

	p := NewVaultProvider(srv.URL, "tok", "")
	got, err := p.Fetch(context.Background(), "kv/data/sorobeacon", "slack_token")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "https://hooks.example/vault" {
		t.Fatalf("got %q", got)
	}
	if gotToken != "tok" {
		t.Fatalf("token header = %q", gotToken)
	}
}

func TestVaultProviderKVv1AndSingleField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"secret":"abc123"}}`))
	}))
	defer srv.Close()

	p := NewVaultProvider(srv.URL, "", "")
	// No key given: a single-field secret resolves without naming it.
	got, err := p.Fetch(context.Background(), "secret/sorobeacon", "")
	if err != nil || got != "abc123" {
		t.Fatalf("Fetch = (%q, %v)", got, err)
	}
}

func TestVaultProviderErrorsDoNotLeakBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]} super-secret-value`))
	}))
	defer srv.Close()

	p := NewVaultProvider(srv.URL, "", "")
	_, err := p.Fetch(context.Background(), "kv/data/sorobeacon", "k")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should mention the status", err)
	}
	if strings.Contains(err.Error(), "super-secret-value") || strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error leaked the response body: %q", err)
	}

	// A missing field names the field but not the value.
	if _, err := p.Fetch(context.Background(), "kv/data/x", ""); err == nil {
		// The server above returns a status error for any path, so this
		// branch is unreachable; kept for clarity of intent.
		t.Fatalf("unexpected success")
	}
}

func TestVaultProviderMissingField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"data":{"present":"yes"}}}`))
	}))
	defer srv.Close()

	p := NewVaultProvider(srv.URL, "", "")
	_, err := p.Fetch(context.Background(), "kv/data/x", "absent")
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("error = %v, want one naming the missing field", err)
	}
}

func TestVaultProviderUnconfiguredAddress(t *testing.T) {
	p := NewVaultProvider("", "", "")
	if _, err := p.Fetch(context.Background(), "kv/data/x", "k"); err == nil {
		t.Fatal("expected an error when VAULT_ADDR is empty")
	}
}
