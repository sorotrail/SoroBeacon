package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/config"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// captureSlog returns a JSON logger and the buffer its records land in. The
// record shape lets a test assert a level and a property without pinning the
// exact human-readable wording.
func captureSlog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// logRecords parses every JSON line the logger wrote.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		out = append(out, rec)
	}
	return out
}

func countLevel(t *testing.T, buf *bytes.Buffer, level string) int {
	t.Helper()
	n := 0
	for _, rec := range logRecords(t, buf) {
		if rec["level"] == level {
			n++
		}
	}
	return n
}

// TestBuildSecretResolverEnv proves the env branch constructs a working
// resolver and that a resolved value never reaches the log.
func TestBuildSecretResolverEnv(t *testing.T) {
	const (
		variable = "SOROBEACON_TEST_CHANNEL_WEBHOOK"
		value    = "https://hooks.example/from-env"
	)
	t.Setenv(variable, value)

	log, logs := captureSlog()
	resolver := buildSecretResolver(config.Config{
		SecretsProvider: "env",
		SecretsCacheTTL: time.Minute,
	}, log)
	require.NotNil(t, resolver)

	got, err := resolver.ResolveString(context.Background(), "${secret:env:"+variable+"}")
	require.NoError(t, err)
	assert.Equal(t, value, got)

	assert.Contains(t, logs.String(), "external secrets enabled")
	assert.NotContains(t, logs.String(), value, "resolved secret values must never be logged")
}

type vaultCall struct {
	path      string
	token     string
	namespace string
}

// TestBuildSecretResolverVault proves the vault branch sends the configured
// address, token and namespace, understands a KV v2 response, and keeps both
// the token and the resolved value out of the log.
func TestBuildSecretResolverVault(t *testing.T) {
	const (
		token  = "hvs.recognisable-fake-token"
		secret = "vault-resolved-webhook-value"
	)
	calls := make(chan vaultCall, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- vaultCall{
			path:      r.URL.Path,
			token:     r.Header.Get("X-Vault-Token"),
			namespace: r.Header.Get("X-Vault-Namespace"),
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"slack_token":%q}}}`, secret)
	}))
	t.Cleanup(srv.Close)

	log, logs := captureSlog()
	resolver := buildSecretResolver(config.Config{
		SecretsProvider: "vault",
		SecretsCacheTTL: time.Minute,
		VaultAddr:       srv.URL,
		VaultToken:      token,
		VaultNamespace:  "team-a",
	}, log)
	require.NotNil(t, resolver)

	got, err := resolver.ResolveString(context.Background(),
		"${secret:vault:kv/data/sorobeacon#slack_token}")
	require.NoError(t, err)
	assert.Equal(t, secret, got)

	call := <-calls
	assert.Equal(t, "/v1/kv/data/sorobeacon", call.path)
	assert.Equal(t, token, call.token)
	assert.Equal(t, "team-a", call.namespace)

	assert.Contains(t, logs.String(), "external secrets enabled")
	assert.NotContains(t, logs.String(), token, "the Vault token is a credential")
	assert.NotContains(t, logs.String(), secret, "resolved secret values must never be logged")
}

// TestBuildSecretResolverVaultFailureNamesReferenceNotBody pins the
// credential-safe error contract: a provider failure names the reference that
// failed, never the response body it came from.
func TestBuildSecretResolverVaultFailureNamesReferenceNotBody(t *testing.T) {
	const bodySecret = "response-body-must-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"errors":[%q]}`, bodySecret)
	}))
	t.Cleanup(srv.Close)

	log, _ := captureSlog()
	resolver := buildSecretResolver(config.Config{
		SecretsProvider: "vault",
		SecretsCacheTTL: time.Minute,
		VaultAddr:       srv.URL,
		VaultToken:      "token",
	}, log)
	require.NotNil(t, resolver)

	const reference = "${secret:vault:kv/data/sorobeacon#slack_token}"
	_, err := resolver.ResolveString(context.Background(), reference)
	require.Error(t, err)
	assert.Contains(t, err.Error(), reference)
	assert.NotContains(t, err.Error(), bodySecret)
}

// TestBuildSecretResolverVaultWithoutAddressDefersFailure records a finding:
// the helper itself does not fail fast on a misconfigured backend. It returns
// a resolver and the error surfaces on the first resolve, naming the
// reference. config.Load rejects an empty VAULT_ADDR before this helper is
// called, so operators do see that specific misconfiguration at startup; an
// unreachable Vault is not probed at all.
func TestBuildSecretResolverVaultWithoutAddressDefersFailure(t *testing.T) {
	log, _ := captureSlog()
	resolver := buildSecretResolver(config.Config{
		SecretsProvider: "vault",
		SecretsCacheTTL: time.Minute,
	}, log)
	require.NotNil(t, resolver, "construction does not fall back to another provider")

	const reference = "${secret:vault:kv/data/sorobeacon#slack_token}"
	_, err := resolver.ResolveString(context.Background(), reference)
	require.Error(t, err)
	assert.Contains(t, err.Error(), reference)
	assert.Contains(t, err.Error(), "address is not configured")
}

// TestBuildSecretResolverDisabledIsNil pins the default and the unknown-name
// path. With nothing configured, references stay literal — the pre-existing
// behaviour. config.Load rejects an unknown SECRETS_PROVIDER before startup,
// so the nil returned for "bogus" is unreachable in production; it is pinned
// here so a future caller cannot mistake it for silent degradation.
func TestBuildSecretResolverDisabledIsNil(t *testing.T) {
	for _, provider := range []string{"", "bogus"} {
		t.Run(fmt.Sprintf("%q", provider), func(t *testing.T) {
			log, logs := captureSlog()
			resolver := buildSecretResolver(config.Config{SecretsProvider: provider}, log)
			assert.Nil(t, resolver)
			assert.NotContains(t, logs.String(), "external secrets enabled")
		})
	}
}

// TestWarnIfAuthDisabledFiresAtWarnLevel asserts the condition, not the
// wording, and that the level is visible by default — a debug-level warning
// would be filtered out of exactly the deployment that needs it.
func TestWarnIfAuthDisabledFiresAtWarnLevel(t *testing.T) {
	log, logs := captureSlog()
	warnIfAuthDisabled(log, false)

	records := logRecords(t, logs)
	require.Len(t, records, 1)
	assert.Equal(t, "WARN", records[0]["level"])
	msg, _ := records[0]["msg"].(string)
	assert.Contains(t, strings.ToLower(msg), "authentication is disabled")
}

func TestWarnIfAuthDisabledIsSilentWhenEnabled(t *testing.T) {
	log, logs := captureSlog()
	warnIfAuthDisabled(log, true)
	assert.Zero(t, countLevel(t, logs, "WARN"))
}

// fakeOIDCDiscovery serves the minimum discovery document go-oidc needs so a
// real OIDCProvider can be constructed without contacting a provider.
func fakeOIDCDiscovery(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"issuer": %q,
			"authorization_endpoint": %q,
			"token_endpoint": %q,
			"jwks_uri": %q,
			"response_types_supported": ["code"],
			"subject_types_supported": ["public"],
			"id_token_signing_alg_values_supported": ["RS256"]
		}`, srv.URL, srv.URL+"/authorize", srv.URL+"/token", srv.URL+"/keys")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWarnIfAuthDisabledTracksEffectiveAuthenticatorState covers the cases
// the one-line helper is protecting: open by omission, open because the only
// configured token was blank or mapped to an invalid workspace, closed by a
// static token, and closed by OIDC alone. The partially-configured cases are
// the ones most likely to leave a deployment open while looking set up.
func TestWarnIfAuthDisabledTracksEffectiveAuthenticatorState(t *testing.T) {
	oidcProvider, err := auth.NewOIDCProvider(context.Background(), auth.OIDCConfig{
		Issuer:      fakeOIDCDiscovery(t).URL,
		ClientID:    "sorobeacon-test",
		RedirectURL: "https://sorobeacon.example/oidc/callback",
		Workspace:   workspace.Default,
	})
	require.NoError(t, err)

	const ttl = time.Hour
	tests := []struct {
		name     string
		authn    *auth.Authenticator
		wantWarn bool
	}{
		{
			name:     "no credentials",
			authn:    auth.NewBound(nil, ttl),
			wantWarn: true,
		},
		{
			name: "static token",
			authn: auth.NewBound([]auth.Binding{
				{Workspace: workspace.Default, Token: "static-token"},
			}, ttl),
			wantWarn: false,
		},
		{
			name: "blank static token",
			authn: auth.NewBound([]auth.Binding{
				{Workspace: workspace.Default, Token: "   "},
			}, ttl),
			wantWarn: true,
		},
		{
			name: "invalid workspace",
			authn: auth.NewBound([]auth.Binding{
				{Workspace: workspace.ID("Not/Valid"), Token: "static-token"},
			}, ttl),
			wantWarn: true,
		},
		{
			name:     "oidc only",
			authn:    auth.NewBound(nil, ttl).WithOIDC(oidcProvider),
			wantWarn: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, logs := captureSlog()
			warnIfAuthDisabled(log, tt.authn.Enabled())
			if tt.wantWarn {
				assert.Equal(t, 1, countLevel(t, logs, "WARN"))
			} else {
				assert.Zero(t, countLevel(t, logs, "WARN"))
			}
		})
	}
}
