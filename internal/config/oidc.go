// Single sign-on settings: the OIDC_* variables, and the validation that turns
// them into something safe to build a login flow from.
//
// It lives in its own file for the same reason workspace_tokens.go does:
// config.go holds plain configuration, and the derived view over these fields
// (Enabled, and the mapping into auth.OIDCConfig) belongs next to the rules
// that make them consistent.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// OIDC is one OpenID Connect provider for the dashboard's sign-in page. The
// zero value — and any value with an empty Issuer — means SSO is off, which is
// how a deployment that never set an OIDC_ variable behaves: the token form at
// /login is the only way in, exactly as before this existed.
//
// ClientSecret is a credential and must never be logged or echoed; the rest are
// provider coordinates an operator would paste into a ticket without worry.
type OIDC struct {
	// Issuer is the provider's base URL, where /.well-known/openid-configuration
	// is read from (OIDC_ISSUER). Setting it is what switches SSO on.
	Issuer string
	// ClientID and ClientSecret are this instance's registration
	// (OIDC_CLIENT_ID, OIDC_CLIENT_SECRET). ClientID is checked against the
	// ID token's audience, so the secret is not the only thing standing between
	// another client's tokens and this dashboard.
	ClientID     string
	ClientSecret string
	// RedirectURL is this instance's absolute callback URL (OIDC_REDIRECT_URL).
	// It is configured rather than derived from the Host header so the provider's
	// redirect-URI allow-list stays meaningful behind a proxy.
	RedirectURL string
	// Scopes is the OIDC scope list (OIDC_SCOPES, space- or comma-separated).
	// Empty asks for "openid profile email"; openid is always included.
	Scopes []string
	// Workspace is where a signed-in user lands (OIDC_WORKSPACE, default
	// `default`).
	Workspace workspace.ID
	// WorkspaceClaim names an ID-token claim used as the workspace id instead
	// (OIDC_WORKSPACE_CLAIM), for one instance serving several tenants off one
	// provider. Empty disables claim mapping.
	WorkspaceClaim string
	// AllowedDomains is the e-mail domain allow-list (OIDC_ALLOWED_DOMAINS).
	// Empty accepts every account the provider authenticates.
	AllowedDomains []string
	// StateTTL is how long an unfinished login may be completed
	// (OIDC_LOGIN_STATE_TTL, default 10m).
	StateTTL time.Duration
}

// Enabled reports whether SSO is configured. Issuer alone decides: a provider
// with no client id is a misconfiguration Load rejects, not a half-on state
// any caller has to reason about.
func (o OIDC) Enabled() bool {
	return strings.TrimSpace(o.Issuer) != ""
}

// parseOIDC reads and validates the OIDC_* variables. getenv is injected the
// way ParseNetwork takes it, so the tests drive it with a map rather than the
// process environment.
func parseOIDC(getenv func(string) string) (OIDC, error) {
	o := OIDC{
		Issuer:         strings.TrimRight(strings.TrimSpace(getenv("OIDC_ISSUER")), "/"),
		ClientID:       strings.TrimSpace(getenv("OIDC_CLIENT_ID")),
		ClientSecret:   strings.TrimSpace(getenv("OIDC_CLIENT_SECRET")), // never logged either way
		RedirectURL:    strings.TrimSpace(getenv("OIDC_REDIRECT_URL")),
		Workspace:      workspace.ID(strings.TrimSpace(getenv("OIDC_WORKSPACE"))),
		WorkspaceClaim: strings.TrimSpace(getenv("OIDC_WORKSPACE_CLAIM")),
		Scopes:         splitList(getenv("OIDC_SCOPES")),
		AllowedDomains: lowerList(getenv("OIDC_ALLOWED_DOMAINS")),
	}
	if o.Workspace == "" {
		o.Workspace = workspace.Default
	}

	if o.Issuer == "" {
		// Off — but not silently inconsistent. Every other OIDC_ variable is
		// ignored here, and an operator who set one expects it to matter, so
		// say which variable is doing nothing instead of starting up and
		// leaving the question for whoever wonders why SSO is missing. This is
		// decided before any value is validated: with no provider there is
		// nothing for a TTL or a workspace id to be wrong about, and the
		// variable the operator actually got wrong is the useful answer.
		for _, key := range []string{
			"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URL", "OIDC_SCOPES",
			"OIDC_WORKSPACE", "OIDC_WORKSPACE_CLAIM", "OIDC_ALLOWED_DOMAINS", "OIDC_LOGIN_STATE_TTL",
		} {
			if strings.TrimSpace(getenv(key)) != "" {
				return o, fmt.Errorf("%s is set but OIDC_ISSUER is not, so single sign-on is off: set OIDC_ISSUER to the provider's base URL or unset %s", key, key)
			}
		}
		return OIDC{}, nil
	}

	if !workspace.Valid(string(o.Workspace)) {
		return o, fmt.Errorf("invalid OIDC_WORKSPACE %q: ids are 1-40 characters of [a-z0-9_-]", o.Workspace)
	}
	if v := strings.TrimSpace(getenv("OIDC_LOGIN_STATE_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return o, fmt.Errorf("invalid OIDC_LOGIN_STATE_TTL %q: %w", v, err)
		}
		if d <= 0 {
			return o, fmt.Errorf("OIDC_LOGIN_STATE_TTL %q must be greater than 0", v)
		}
		o.StateTTL = d
	}

	if !strings.HasPrefix(o.Issuer, "http://") && !strings.HasPrefix(o.Issuer, "https://") {
		return o, fmt.Errorf("invalid OIDC_ISSUER %q: must be the provider's absolute http(s) base URL, e.g. https://accounts.example.com", o.Issuer)
	}
	if o.ClientID == "" {
		return o, fmt.Errorf("OIDC_CLIENT_ID is required when OIDC_ISSUER is set")
	}
	if o.RedirectURL == "" {
		return o, fmt.Errorf("OIDC_REDIRECT_URL is required when OIDC_ISSUER is set: this instance's absolute callback URL, which must match the provider's registered redirect URI")
	}
	if !strings.HasPrefix(o.RedirectURL, "http://") && !strings.HasPrefix(o.RedirectURL, "https://") {
		return o, fmt.Errorf("invalid OIDC_REDIRECT_URL %q: must be an absolute http or https URL", o.RedirectURL)
	}
	return o, nil
}

// splitList reads a space- or comma-separated list, dropping blanks. Scopes are
// written space-separated by convention while every other list in this file is
// comma-separated, so both spellings are accepted rather than one of them being
// a startup error an operator has to discover.
func splitList(v string) []string {
	return strings.Fields(strings.ReplaceAll(v, ",", " "))
}

// lowerList is splitList for values whose comparison is case-insensitive, so
// the stored form is already normalised and no caller has to remember to fold.
func lowerList(v string) []string {
	var out []string
	for _, s := range splitList(v) {
		out = append(out, strings.ToLower(s))
	}
	return out
}

// AuthOIDC is this settings block seen from the credential side: the shape
// internal/auth builds a provider from. Like AuthBindings it exists so the
// mapping lives with the rules, and main's wire-up stays one call.
func (o OIDC) AuthOIDC() auth.OIDCConfig {
	return auth.OIDCConfig{
		Issuer:         o.Issuer,
		ClientID:       o.ClientID,
		ClientSecret:   o.ClientSecret,
		RedirectURL:    o.RedirectURL,
		Scopes:         o.Scopes,
		Workspace:      o.Workspace,
		WorkspaceClaim: o.WorkspaceClaim,
		AllowedDomains: o.AllowedDomains,
		StateTTL:       o.StateTTL,
	}
}
