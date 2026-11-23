package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/oidctest"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// The OIDC tests drive a real provider (internal/oidctest: discovery, JWKS,
// signed RS256 ID tokens, and a token endpoint that enforces client
// credentials, single-use codes and PKCE). Nothing here stubs the verification,
// because the verification is the feature.

const testRedirect = "https://beacon.example.com/login/oidc/callback"

func newTestProvider(t *testing.T, idp *oidctest.Provider, mutate func(*OIDCConfig)) *OIDCProvider {
	t.Helper()
	cfg := OIDCConfig{
		Issuer:       idp.Issuer(),
		ClientID:     idp.ClientID,
		ClientSecret: idp.ClientSecret,
		RedirectURL:  testRedirect,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := NewOIDCProvider(ctx, cfg)
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	return p
}

// oidcLogin plays the browser's part between the two handlers: start a login,
// authorize it at the provider, and return the callback query with the state
// the browser was given. Tests that want a broken callback take these and
// damage one field.
func oidcLogin(t *testing.T, p *OIDCProvider, idp *oidctest.Provider, next string) (q url.Values, state, code string) {
	t.Helper()
	start, state, err := p.BeginLogin(next)
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	parsed, err := url.Parse(start)
	if err != nil {
		t.Fatalf("parse start URL: %v", err)
	}
	code, err = idp.Authorize(parsed.Query())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return url.Values{"state": {state}, "code": {code}}, state, code
}

// respond hands the provider the ID token its next exchange should return,
// built from the claims a valid login would carry and edited by mutate.
func respond(t *testing.T, idp *oidctest.Provider, code string, mutate func(map[string]any)) {
	t.Helper()
	claims := idp.ClaimsFor(code)
	if mutate != nil {
		mutate(claims)
	}
	idp.RespondWith(idp.IDToken(claims))
}

func complete(t *testing.T, p *OIDCProvider, q url.Values, state string) (*Identity, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.CompleteLogin(ctx, q, state)
}

func TestNewOIDCProviderRejectsUnusableConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     OIDCConfig
		wantErr string
	}{
		{
			name:    "no issuer",
			cfg:     OIDCConfig{ClientID: "c", RedirectURL: testRedirect},
			wantErr: "issuer is required",
		},
		{
			name:    "issuer without a scheme",
			cfg:     OIDCConfig{Issuer: "accounts.example.com", ClientID: "c", RedirectURL: testRedirect},
			wantErr: "absolute http",
		},
		{
			name:    "no client id",
			cfg:     OIDCConfig{Issuer: "https://accounts.example.com", RedirectURL: testRedirect},
			wantErr: "client id is required",
		},
		{
			name:    "relative redirect",
			cfg:     OIDCConfig{Issuer: "https://accounts.example.com", ClientID: "c", RedirectURL: "/login/oidc/callback"},
			wantErr: "redirect URL",
		},
		{
			name:    "workspace that cannot be named",
			cfg:     OIDCConfig{Issuer: "https://accounts.example.com", ClientID: "c", RedirectURL: testRedirect, Workspace: "Not Valid!"},
			wantErr: "invalid OIDC workspace",
		},
	} {
		_, err := NewOIDCProvider(context.Background(), tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want one mentioning %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestDiscoveryHappensAtConstruction(t *testing.T) {
	// A provider that cannot be reached is a construction failure, not a
	// sign-in failure: the operator finds out during the deploy.
	cfg := OIDCConfig{
		Issuer:      "http://127.0.0.1:1",
		ClientID:    "c",
		RedirectURL: testRedirect,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := NewOIDCProvider(ctx, cfg)
	if err == nil || !strings.Contains(err.Error(), "discover OIDC provider") {
		t.Fatalf("error = %v, want a discovery failure", err)
	}
}

func TestBeginLoginSendsWhatAProviderNeeds(t *testing.T) {
	// Authorize is the provider's own check of the request: client id,
	// response_type, the openid scope, a state, a nonce and an S256 challenge
	// all have to be there for it to hand out a code.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)

	start, state, err := p.BeginLogin("/alerts")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	parsed, err := url.Parse(start)
	if err != nil {
		t.Fatalf("parse start URL: %v", err)
	}
	if got := parsed.Query().Get("redirect_uri"); got != testRedirect {
		t.Errorf("redirect_uri = %q, want the configured callback URL", got)
	}
	// A client secret must never travel through a browser: it belongs in the
	// server-to-server exchange, and this URL is a redirect target.
	if strings.Contains(start, idp.ClientSecret) {
		t.Error("the authorize URL carried the client secret")
	}
	code, err := idp.Authorize(parsed.Query())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	// The challenge sent to the provider is the hash of the verifier held
	// server-side. Without that pairing PKCE is decoration, so this is the one
	// line that proves the S256 option actually hashed the value it sent.
	p.mu.Lock()
	pending := p.pending[state]
	p.mu.Unlock()
	if pending == nil {
		t.Fatal("BeginLogin stored no login state")
	}
	if got, want := parsed.Query().Get("code_challenge"), oidctest.S256Challenge(pending.verifier); got != want {
		t.Errorf("code_challenge = %q, want S256(code_verifier) %q", got, want)
	}
	if pending.next != "/alerts" {
		t.Errorf("stored next = %q, want the target the caller asked for", pending.next)
	}
	if code == "" {
		t.Fatal("no code issued")
	}
}

func TestCompleteLoginAcceptsAValidIdentity(t *testing.T) {
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)

	q, state, code := oidcLogin(t, p, idp, "/monitors")
	respond(t, idp, code, nil)

	id, next, err := complete(t, p, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if id.Subject != "user-123" || id.Email != "ops@example.com" || id.Name != "Ops Person" {
		t.Errorf("identity = %+v, want the claims the provider signed", id)
	}
	if id.Workspace != workspace.Default {
		t.Errorf("workspace = %q, want the default with no mapping configured", id.Workspace)
	}
	// next came back through the server-side state, not the callback query: the
	// callback URL the provider redirects to carries no such parameter.
	if next != "/monitors" {
		t.Errorf("next = %q, want /monitors", next)
	}
	// The exchange is where the confidential client authenticates, and where
	// the verifier proves it started this login.
	exchange := idp.LastExchange()
	if exchange.Get("client_id") != idp.ClientID || exchange.Get("client_secret") != idp.ClientSecret {
		t.Error("the code exchange did not carry the client credentials")
	}
	if exchange.Get("code_verifier") == "" {
		t.Error("the code exchange sent no PKCE verifier")
	}
}

func TestStateIsSingleUse(t *testing.T) {
	// A captured callback URL is a one-time credential. If the state survived
	// its first use, whoever read it off a proxy log could walk in again.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)

	q, state, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, nil)
	if _, _, err := complete(t, p, q, state); err != nil {
		t.Fatalf("first CompleteLogin: %v", err)
	}
	// The mechanism, checked directly: the entry is gone, so nothing can use
	// that state again regardless of what the provider still honours.
	if p.hasPending(state) {
		t.Fatal("the login state survived its first use")
	}
	// And the consequence: replaying the same callback, with a fresh token still
	// available at the provider, is refused.
	respond(t, idp, code, nil)
	if _, _, err := complete(t, p, q, state); !errors.Is(err, ErrOIDCLogin) {
		t.Fatalf("replayed state = %v, want ErrOIDCLogin", err)
	}
}

func TestExchangeWithTheWrongVerifierFails(t *testing.T) {
	// PKCE is the reason a code intercepted in transit is useless. The fake
	// provider enforces the verifier, so this is the case where the exchange
	// carries everything except proof that the caller started the login.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)
	_, _, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption("not-the-verifier")); err == nil {
		t.Fatal("the provider accepted an exchange with the wrong PKCE verifier")
	}
}

func TestCompleteLoginFailureBranches(t *testing.T) {
	// Every way a callback can be wrong, and the one answer for most of them.
	// The point of the table is that a *forged* and a *malformed* callback are
	// indistinguishable to whoever sent it: both get ErrOIDCLogin, and the
	// reasons differ only in the log line an operator reads.
	type broken struct {
		name    string
		mutate  func(map[string]any)
		tweak   func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string)
		wantErr string
	}
	cases := []broken{
		{
			name:    "signature from a key the provider does not publish",
			wantErr: "id_token rejected",
			tweak: func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string) {
				claims := idp.ClaimsFor(q.Get("code"))
				idp.RespondWith(idp.IDTokenWith(idp.OtherKey(), claims))
				return q, state
			},
		},
		{
			name:    "signed for a different issuer",
			wantErr: "id_token rejected",
			mutate:  func(c map[string]any) { c["iss"] = "https://somewhere.else" },
		},
		{
			name:    "audience is another client",
			wantErr: "id_token rejected",
			mutate:  func(c map[string]any) { c["aud"] = "not-our-client" },
		},
		{
			name:    "expired",
			wantErr: "id_token rejected",
			mutate: func(c map[string]any) {
				c["exp"] = time.Now().Add(-time.Hour).Unix()
				c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
			},
		},
		{
			// The replay defence, and the one check go-oidc leaves to the
			// caller. A token minted for a different login is otherwise
			// completely valid: right key, right issuer, right audience, not
			// expired. Remove the nonce comparison from CompleteLogin and this
			// case starts signing people in.
			name:    "nonce from another login",
			wantErr: "nonce mismatch",
			mutate:  func(c map[string]any) { c["nonce"] = "a-nonce-nobody-asked-for" },
		},
		{
			name:    "no sub claim",
			wantErr: "no sub claim",
			mutate:  func(c map[string]any) { delete(c, "sub") },
		},
		{
			name: "provider returned no id_token",
			tweak: func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string) {
				idp.RespondWithoutIDToken()
				return q, state
			},
			wantErr: "no id_token",
		},
		{
			name: "provider refuses the exchange",
			tweak: func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string) {
				idp.FailExchange = true
				return q, state
			},
			wantErr: "code exchange",
		},
		{
			name:    "unknown state",
			wantErr: ErrOIDCLogin.Error(),
			tweak: func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string) {
				return url.Values{"state": {"never-issued"}, "code": {q.Get("code")}}, state
			},
		},
		{
			name:    "no state at all",
			wantErr: ErrOIDCLogin.Error(),
			tweak: func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string) {
				return url.Values{"code": {q.Get("code")}}, state
			},
		},
		{
			name:    "state not the one this browser was given",
			wantErr: ErrOIDCLogin.Error(),
			tweak: func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string) {
				// A login CSRF: the attacker completed their own provider
				// round-trip and is trying to get the victim to land on it.
				return url.Values{"state": {"attacker-owned"}, "code": {q.Get("code")}}, state
			},
		},
		{
			name:    "browser presented no cookie",
			wantErr: ErrOIDCLogin.Error(),
			tweak: func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string) {
				return q, ""
			},
		},
		{
			name:    "callback carries no code",
			wantErr: ErrOIDCLogin.Error(),
			tweak: func(t *testing.T, idp *oidctest.Provider, q url.Values, state string) (url.Values, string) {
				return url.Values{"state": {state}}, state
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := oidctest.New(t)
			p := newTestProvider(t, idp, nil)
			q, state, code := oidcLogin(t, p, idp, "/")
			if tc.mutate != nil {
				respond(t, idp, code, tc.mutate)
			}
			if tc.tweak != nil {
				q, state = tc.tweak(t, idp, q, state)
			}
			_, _, err := complete(t, p, q, state)
			if err == nil {
				t.Fatal("CompleteLogin accepted a callback it should have refused")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestProviderErrorFromTheCallbackIsReportedAsIs(t *testing.T) {
	// The provider's own "the user clicked cancel" is not a forgery symptom, and
	// an operator needs to be able to tell the two apart in one line.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)
	q, state, _ := oidcLogin(t, p, idp, "/")
	q.Set("error", "access_denied")
	_, _, err := complete(t, p, q, state)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("error = %v, want the provider's own reason", err)
	}
	if errors.Is(err, ErrOIDCLogin) {
		t.Error("a provider refusal must not be classed as a failed verification")
	}
}

func TestAllowedDomainsGate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		email    any
		domains  []string
		wantErr  bool
		wantText string
	}{
		{name: "listed domain", email: "ops@example.com", domains: []string{"example.com"}},
		{name: "second listed domain", email: "ops@other.test", domains: []string{"example.com", "other.test"}},
		{name: "unlisted domain", email: "someone@evil.test", domains: []string{"example.com"}, wantErr: true, wantText: "not in an allowed domain"},
		{name: "no email claim at all", email: nil, domains: []string{"example.com"}, wantErr: true, wantText: "no email claim"},
		// Case: providers fold addresses differently, and the comparison is
		// configured to be case-insensitive.
		{name: "mixed case address", email: "Ops@EXAMPLE.com", domains: []string{"example.com"}},
		{name: "no allow-list configured accepts anyone the provider does", email: "anyone@anywhere.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idp := oidctest.New(t)
			p := newTestProvider(t, idp, func(cfg *OIDCConfig) { cfg.AllowedDomains = tc.domains })
			q, state, code := oidcLogin(t, p, idp, "/")
			respond(t, idp, code, func(c map[string]any) {
				if tc.email == nil {
					delete(c, "email")
					return
				}
				c["email"] = tc.email
			})
			id, _, err := complete(t, p, q, state)
			switch {
			case tc.wantErr && err == nil:
				t.Errorf("accepted %+v, want a refusal", id)
			case tc.wantErr:
				if !errors.Is(err, ErrOIDCAccount) {
					t.Errorf("error = %v, want ErrOIDCAccount", err)
				}
				if !strings.Contains(err.Error(), tc.wantText) {
					t.Errorf("error = %q, want it to mention %q", err, tc.wantText)
				}
			case err != nil:
				t.Fatalf("CompleteLogin: %v", err)
			}
		})
	}
}

func TestWorkspaceMappingFromAClaim(t *testing.T) {
	// One provider, one instance, several tenants: the claim is trusted because
	// the provider signed it, and an unusable value falls back to the workspace
	// the operator configured rather than locking the user out.
	for _, tc := range []struct {
		name  string
		claim any
		want  workspace.ID
	}{
		{name: "claim names a workspace", claim: "acme", want: "acme"},
		{name: "claim is not a workspace id", claim: "Not Valid!", want: workspace.Default},
		{name: "claim is not a string", claim: 7, want: workspace.Default},
		{name: "claim is absent", claim: nil, want: workspace.Default},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idp := oidctest.New(t)
			p := newTestProvider(t, idp, func(cfg *OIDCConfig) { cfg.WorkspaceClaim = "workspace" })
			q, state, code := oidcLogin(t, p, idp, "/")
			respond(t, idp, code, func(c map[string]any) {
				if tc.claim == nil {
					delete(c, "workspace")
					return
				}
				c["workspace"] = tc.claim
			})
			id, _, err := complete(t, p, q, state)
			if err != nil {
				t.Fatalf("CompleteLogin: %v", err)
			}
			if id.Workspace != tc.want {
				t.Errorf("workspace = %q, want %q", id.Workspace, tc.want)
			}
		})
	}
}

func TestWorkspaceMappingIsOffUnlessConfigured(t *testing.T) {
	// A provider that happens to publish a "workspace" claim must not move
	// users around until an operator asks for it.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)
	q, state, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, func(c map[string]any) { c["workspace"] = "acme" })
	id, _, err := complete(t, p, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if id.Workspace != workspace.Default {
		t.Errorf("workspace = %q, want the configured default with no claim mapping", id.Workspace)
	}
}

func TestUnfinishedLoginExpires(t *testing.T) {
	// Someone who opens the provider page and walks away leaves a nonce and a
	// verifier in memory. They should not sit there forever.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, func(cfg *OIDCConfig) { cfg.StateTTL = time.Minute })
	clock := time.Unix(1_800_000_000, 0).UTC()
	p.withNow(func() time.Time { return clock })

	// A login that finishes well inside the TTL works.
	q, state, code := oidcLogin(t, p, idp, "/")
	respond(t, idp, code, nil)
	if _, _, err := complete(t, p, q, state); err != nil {
		t.Fatalf("within the TTL: %v", err)
	}

	// Start one and never finish it; past the TTL the same state is dead, even
	// though the provider would still answer for the code.
	q2, state2, code2 := oidcLogin(t, p, idp, "/")
	respond(t, idp, code2, nil)
	clock = clock.Add(2 * time.Minute)
	if _, _, err := complete(t, p, q2, state2); !errors.Is(err, ErrOIDCLogin) {
		t.Errorf("an expired login = %v, want ErrOIDCLogin", err)
	}
	// The sweep that made it dead also kept the map from growing, and a login
	// started after it still completes.
	q3, state3, code3 := oidcLogin(t, p, idp, "/")
	respond(t, idp, code3, nil)
	if _, _, err := complete(t, p, q3, state3); err != nil {
		t.Errorf("a fresh login after expiry = %v, want success", err)
	}
	if n := pendingCount(p); n != 0 {
		t.Errorf("%d unfinished logins left in memory, want the sweep to have taken them", n)
	}
}

// hasPending reports whether an unfinished login is still stored under a state.
func (p *OIDCProvider) hasPending(state string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.pending[state]
	return ok
}

// pendingCount is the unfinished-login map, read for the sweep assertion.
func pendingCount(p *OIDCProvider) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

func TestProviderWithoutAuthenticatorStillDescribesItself(t *testing.T) {
	// The nil provider is the unconfigured deployment, so it answers rather
	// than panicking wherever a handler asks it a question.
	var p *OIDCProvider
	if p.Enabled() {
		t.Error("a nil provider must not read as configured")
	}
	if p.DisplayName() != "" {
		t.Error("a nil provider has no name to show")
	}
	if _, _, err := p.BeginLogin("/"); err == nil {
		t.Error("BeginLogin on a nil provider must refuse")
	}
	if _, _, err := p.CompleteLogin(context.Background(), url.Values{"state": {"x"}, "code": {"y"}}, "x"); !errors.Is(err, ErrOIDCLogin) {
		t.Errorf("CompleteLogin = %v, want ErrOIDCLogin", err)
	}
	if c := p.StateCookie("abc", true); c.Value != "abc" || !c.Secure {
		t.Errorf("StateCookie on a nil provider = %+v, want the documented default", c)
	}
}

func TestStateCookieCarriesTheLoginNotTheCredential(t *testing.T) {
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)
	_, state, _ := oidcLogin(t, p, idp, "/")

	c := p.StateCookie(state, true)
	if c.Name != StateCookie || c.Value != state {
		t.Fatalf("cookie = %+v", c)
	}
	if !c.HttpOnly || c.Path != "/" || !c.Secure {
		t.Errorf("cookie flags = %+v, want HttpOnly, Path=/ and Secure when the request was TLS", c)
	}
	// Lax, not Strict: the provider finishes with a top-level GET navigation,
	// and Strict would withhold the cookie from exactly the request that needs
	// it, so sign-in could never complete.
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want a positive lifetime", c.MaxAge)
	}
	cleared := p.ClearStateCookie(false)
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("clear cookie = %+v, want an expired empty cookie", cleared)
	}
}

func TestScopesAlwaysAskForOpenID(t *testing.T) {
	// Without `openid` there is no ID token at all, and the failure surfaces at
	// the first sign-in rather than at startup — so the scope is added here.
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{in: nil, want: []string{"openid"}},
		{in: []string{"email", "profile"}, want: []string{"openid", "email", "profile"}},
		{in: []string{"openid email", "email", " ", "offline_access"}, want: []string{"openid", "email", "offline_access"}},
	} {
		got := normaliseScopes(tc.in)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("normaliseScopes(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The same rule at the wire: a config that forgot openid still asks for it.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, func(cfg *OIDCConfig) { cfg.Scopes = []string{"email"} })
	start, _, err := p.BeginLogin("/")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	parsed, err := url.Parse(start)
	if err != nil {
		t.Fatalf("parse start URL: %v", err)
	}
	if !strings.Contains(parsed.Query().Get("scope"), "openid") {
		t.Errorf("the authorize request asked for %q, which cannot return an id_token", parsed.Query().Get("scope"))
	}
}

func TestAuthenticatorAcceptsSSOAsACredentialSource(t *testing.T) {
	// The dashboard must not sit open on the port while an operator is moving to
	// SSO: configuring a provider alone makes authentication on.
	idp := oidctest.New(t)
	p := newTestProvider(t, idp, nil)
	a := NewBound([]Binding{{Workspace: "acme", Token: "t-acme"}}, 0)
	if !a.HasStaticTokens() {
		t.Error("static tokens are configured, and HasStaticTokens disagrees")
	}
	// Local login keeps working beside SSO: that is the migration path, not a
	// fallback an operator loses by turning the provider on.
	a.WithOIDC(p)
	if !a.Enabled() {
		t.Fatal("Enabled = false with a provider configured")
	}
	if id, ok := a.Login("t-acme"); !ok || !a.HasSession(id) {
		t.Error("a static token no longer signs in once SSO is configured")
	}

	// An SSO-only instance: no static token at all.
	b := New(nil, 0).WithOIDC(p)
	if !b.Enabled() {
		t.Fatal("Enabled = false for an SSO-only deployment")
	}
	if b.HasStaticTokens() {
		t.Error("an SSO-only deployment has no token form to offer")
	}
	if _, ok := b.Login("anything"); ok {
		t.Error("a token must not sign in where none is configured")
	}

	// A verified identity becomes a session in *its* workspace, and that is the
	// only way in: LoginOIDC takes the Identity, not a caller-chosen workspace.
	mapped := newTestProvider(t, idp, func(cfg *OIDCConfig) { cfg.WorkspaceClaim = "workspace" })
	q, state, code := oidcLogin(t, mapped, idp, "/")
	respond(t, idp, code, func(c map[string]any) { c["workspace"] = "acme" })
	id, _, err := complete(t, mapped, q, state)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	sid := b.LoginOIDC(id)
	if sid == "" {
		t.Fatal("LoginOIDC minted no session")
	}
	if ws, ok := b.SessionWorkspace(sid); !ok || ws != "acme" {
		t.Errorf("session workspace = %q (%v), want acme", ws, ok)
	}
	if b.LoginOIDC(nil) != "" {
		t.Error("no identity must not produce a session")
	}
}
