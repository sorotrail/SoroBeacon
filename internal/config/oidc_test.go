package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// The OIDC_* variables. Two things dominate these tests: an instance that never
// mentions SSO must come out exactly as it did before SSO existed, and an
// instance that mentions it half-way must be told at startup instead of running
// with a sign-in page that cannot work.

// oidcEnv clears every SSO variable so a value leaked from the developer's
// shell cannot be mistaken for the unset path, then applies the ones given.
func oidcEnv(t *testing.T, env map[string]string) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, key := range []string{
		"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URL",
		"OIDC_SCOPES", "OIDC_WORKSPACE", "OIDC_WORKSPACE_CLAIM",
		"OIDC_ALLOWED_DOMAINS", "OIDC_LOGIN_STATE_TTL",
	} {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
}

const oidcBase = "https://accounts.example.com/realms/soro/"

func loadWithOIDC(t *testing.T, env map[string]string) Config {
	t.Helper()
	oidcEnv(t, env)
	cfg, err := Load()
	require.NoError(t, err)
	return cfg
}

func TestLoadSingleSignOnIsOffByDefault(t *testing.T) {
	cfg := loadWithOIDC(t, nil)
	assert.False(t, cfg.OIDC.Enabled())
	assert.Equal(t, OIDC{}, cfg.OIDC)
	// The whole point of the gate: with no provider, the tenant list and the
	// startup line are what they were before this feature shipped.
	assert.Equal(t, []workspace.ID{workspace.Default}, cfg.Workspaces())
	assert.NotContains(t, logLine(cfg), "sso_enabled=true")
}

// TestLoadRejectsSSOSettingsWithoutIssuer covers the half-configured case one
// variable at a time. Every one of them is a credential or a coordinate the
// operator typed expecting it to matter, so the answer is an error naming the
// variable rather than a silent off.
func TestLoadRejectsSSOSettingsWithoutIssuer(t *testing.T) {
	for _, key := range []string{
		"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URL", "OIDC_SCOPES",
		"OIDC_WORKSPACE", "OIDC_WORKSPACE_CLAIM", "OIDC_ALLOWED_DOMAINS", "OIDC_LOGIN_STATE_TTL",
	} {
		t.Run(key, func(t *testing.T) {
			oidcEnv(t, map[string]string{key: "anything"})
			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
			assert.Contains(t, err.Error(), "OIDC_ISSUER")
		})
	}
}

func TestLoadSingleSignOn(t *testing.T) {
	cfg := loadWithOIDC(t, map[string]string{
		"OIDC_ISSUER":          oidcBase,
		"OIDC_CLIENT_ID":       "beacon-web",
		"OIDC_CLIENT_SECRET":   "sso-secret",
		"OIDC_REDIRECT_URL":    "https://beacon.example.com/login/oidc/callback",
		"OIDC_SCOPES":          "openid, email  profile",
		"OIDC_WORKSPACE":       "acme",
		"OIDC_WORKSPACE_CLAIM": "tenant",
		"OIDC_ALLOWED_DOMAINS": "Example.com, ops.EXAMPLE",
		"OIDC_LOGIN_STATE_TTL": "3m",
		"API_TOKEN":            "operator-token",
		"WORKSPACE_TOKENS":     "beta=beta-token",
	})

	require.True(t, cfg.OIDC.Enabled())
	// The trailing slash is part of how people paste an issuer, but a provider
	// compares its own iss to the discovery URL character for character.
	assert.Equal(t, "https://accounts.example.com/realms/soro", cfg.OIDC.Issuer)
	assert.Equal(t, "beacon-web", cfg.OIDC.ClientID)
	assert.Equal(t, "sso-secret", cfg.OIDC.ClientSecret)
	assert.Equal(t, "https://beacon.example.com/login/oidc/callback", cfg.OIDC.RedirectURL)
	assert.Equal(t, []string{"openid", "email", "profile"}, cfg.OIDC.Scopes)
	assert.Equal(t, workspace.ID("acme"), cfg.OIDC.Workspace)
	assert.Equal(t, "tenant", cfg.OIDC.WorkspaceClaim)
	// Domains are compared case-insensitively, so the stored form is already
	// folded and no comparison has to remember to.
	assert.Equal(t, []string{"example.com", "ops.example"}, cfg.OIDC.AllowedDomains)
	assert.Equal(t, 3*time.Minute, cfg.OIDC.StateTTL)

	// A configured tenant is one this instance serves, so it joins the list
	// startup seeds — beside the tenants the tokens name.
	assert.Equal(t, []workspace.ID{workspace.Default, "beta", "acme"}, cfg.Workspaces())

	auth := cfg.OIDC.AuthOIDC()
	assert.Equal(t, cfg.OIDC.Issuer, auth.Issuer)
	assert.Equal(t, cfg.OIDC.ClientID, auth.ClientID)
	assert.Equal(t, cfg.OIDC.ClientSecret, auth.ClientSecret)
	assert.Equal(t, cfg.OIDC.RedirectURL, auth.RedirectURL)
	assert.Equal(t, cfg.OIDC.Scopes, auth.Scopes)
	assert.Equal(t, cfg.OIDC.Workspace, auth.Workspace)
	assert.Equal(t, cfg.OIDC.WorkspaceClaim, auth.WorkspaceClaim)
	assert.Equal(t, cfg.OIDC.AllowedDomains, auth.AllowedDomains)
	assert.Equal(t, cfg.OIDC.StateTTL, auth.StateTTL)
}

func TestLoadSingleSignOnWorkspaceDefaults(t *testing.T) {
	// The minimum viable provider: everything else has a default, because the
	// callback URL is the only coordinate that cannot be derived safely.
	cfg := loadWithOIDC(t, map[string]string{
		"OIDC_ISSUER":       "https://accounts.example.com",
		"OIDC_CLIENT_ID":    "beacon-web",
		"OIDC_REDIRECT_URL": "https://beacon.example.com/login/oidc/callback",
	})
	assert.Equal(t, workspace.Default, cfg.OIDC.Workspace)
	assert.Empty(t, cfg.OIDC.WorkspaceClaim)
	assert.Zero(t, cfg.OIDC.StateTTL)
	assert.Equal(t, []workspace.ID{workspace.Default}, cfg.Workspaces())
}

func TestLoadRejectsUnusableSingleSignOn(t *testing.T) {
	const callback = "https://beacon.example.com/login/oidc/callback"
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "issuer without a scheme",
			env:     map[string]string{"OIDC_ISSUER": "accounts.example.com", "OIDC_CLIENT_ID": "c", "OIDC_REDIRECT_URL": callback},
			wantErr: "OIDC_ISSUER",
		},
		{
			name:    "issuer over an unsupported scheme",
			env:     map[string]string{"OIDC_ISSUER": "ftp://accounts.example.com", "OIDC_CLIENT_ID": "c", "OIDC_REDIRECT_URL": callback},
			wantErr: "OIDC_ISSUER",
		},
		{
			name:    "no client id",
			env:     map[string]string{"OIDC_ISSUER": "https://accounts.example.com", "OIDC_REDIRECT_URL": callback},
			wantErr: "OIDC_CLIENT_ID",
		},
		{
			name:    "no redirect url",
			env:     map[string]string{"OIDC_ISSUER": "https://accounts.example.com", "OIDC_CLIENT_ID": "c"},
			wantErr: "OIDC_REDIRECT_URL",
		},
		{
			// A site-absolute path would be derivable from the request, which is
			// the thing this setting exists to avoid trusting.
			name:    "relative redirect url",
			env:     map[string]string{"OIDC_ISSUER": "https://accounts.example.com", "OIDC_CLIENT_ID": "c", "OIDC_REDIRECT_URL": "/login/oidc/callback"},
			wantErr: "OIDC_REDIRECT_URL",
		},
		{
			name:    "workspace id outside the grammar",
			env:     map[string]string{"OIDC_ISSUER": "https://accounts.example.com", "OIDC_CLIENT_ID": "c", "OIDC_REDIRECT_URL": callback, "OIDC_WORKSPACE": "Acme Corp"},
			wantErr: "OIDC_WORKSPACE",
		},
		{
			name:    "state ttl is not a duration",
			env:     map[string]string{"OIDC_ISSUER": "https://accounts.example.com", "OIDC_CLIENT_ID": "c", "OIDC_REDIRECT_URL": callback, "OIDC_LOGIN_STATE_TTL": "ten minutes"},
			wantErr: "OIDC_LOGIN_STATE_TTL",
		},
		{
			name:    "state ttl is zero",
			env:     map[string]string{"OIDC_ISSUER": "https://accounts.example.com", "OIDC_CLIENT_ID": "c", "OIDC_REDIRECT_URL": callback, "OIDC_LOGIN_STATE_TTL": "0s"},
			wantErr: "OIDC_LOGIN_STATE_TTL",
		},
		{
			name:    "state ttl is negative",
			env:     map[string]string{"OIDC_ISSUER": "https://accounts.example.com", "OIDC_CLIENT_ID": "c", "OIDC_REDIRECT_URL": callback, "OIDC_LOGIN_STATE_TTL": "-5m"},
			wantErr: "OIDC_LOGIN_STATE_TTL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oidcEnv(t, tc.env)
			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestLogAttrsHidesTheSSOClientSecret is the reason fields are opted into the
// startup line one by one: the OIDC block holds a credential, so the line says
// that SSO is on and stops there.
func TestLogAttrsHidesTheSSOClientSecret(t *testing.T) {
	cfg := loadWithOIDC(t, map[string]string{
		"OIDC_ISSUER":        "https://accounts.example.com",
		"OIDC_CLIENT_ID":     "beacon-web",
		"OIDC_CLIENT_SECRET": "super-secret-value",
		"OIDC_REDIRECT_URL":  "https://beacon.example.com/login/oidc/callback",
	})
	line := logLine(cfg)
	assert.Contains(t, line, "sso_enabled=true")
	assert.NotContains(t, line, "super-secret-value")
	assert.NotContains(t, line, "client_secret")
}

// logLine is LogAttrs seen as an operator sees it: one key=value per line.
func logLine(cfg Config) string {
	var dump strings.Builder
	for _, a := range cfg.LogAttrs() {
		dump.WriteString(a.Key)
		dump.WriteByte('=')
		dump.WriteString(a.Value.String())
		dump.WriteByte('\n')
	}
	return dump.String()
}
