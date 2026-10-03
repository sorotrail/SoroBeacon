package auth

// OpenID Connect single sign-on for the dashboard.
//
// SoroBeacon has no user accounts and this file does not give it any: an OIDC
// sign-in asserts *who* the caller is, and the answer is turned into the same
// in-memory session the token form at /login mints. That is what keeps the
// change additive — every route, scope and tenancy rule downstream is looking
// at a session, and does not care which credential produced it.
//
// The flow is authorisation code with PKCE. Implicit (token in the URL
// fragment) was the alternative, and it is worse in three specific ways: the
// credential lands in browser history and any `Referer`-leaking page instead of
// over a server-to-server POST, there is no authorisation code to exchange so
// nothing proves the token came to *this* redirect target, and there is no
// refresh token. A server-rendered dashboard with a client secret of its own
// has no reason to accept that, and PKCE (RFC 7636) is what closes the
// interception hole that the code flow alone leaves open on a browser.
//
// Nonce is not optional here. `state` binds the *browser* to the login it
// started; `nonce` binds the *ID token* to the login, so a token the provider
// handed out for some other purpose — or an earlier, still-valid one — cannot
// be replayed against the callback and turned into a session.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const (
	// StateCookie carries the `state` value of a login in flight, so the
	// callback can prove the browser that arrives is the browser that started.
	// Server-side state alone is not enough: an attacker who can get a victim's
	// browser to the callback with the attacker's own state would otherwise be
	// handing the victim a session for the attacker's identity (login CSRF).
	StateCookie = "sorobeacon_oidc_state"

	// DefaultLoginStateTTL is how long an unfinished OIDC login stays valid.
	// It has to cover a slow provider and a user who stops to pick an account;
	// it has to be short because the entry is a nonce and a PKCE verifier
	// sitting in memory waiting to be used.
	DefaultLoginStateTTL = 10 * time.Minute

	// oidcRandomBytes is the entropy for a state, a nonce and a PKCE verifier:
	// each is a single-use value an attacker may get many guesses at.
	oidcRandomBytes = 32

	// DefaultOIDCScopes is what the provider is asked for. `openid` is what
	// makes the response an ID token at all; `email` is only useful when an
	// allow-list is configured, and `profile` for the name on the sign-in log
	// line, so both are harmless extras rather than required claims.
	DefaultOIDCScopes = "openid profile email"
)

// ErrOIDCLogin is what a login that cannot be completed reports: the state is
// unknown, expired, already used, or not the one this browser was given. Every
// one of those gets the same value on purpose — the differences are about
// internal bookkeeping, and a caller probing the callback learns nothing about
// which check stopped it.
var ErrOIDCLogin = errors.New("auth: sign-in attempt could not be verified")

// ErrOIDCAccount reports that a correctly verified identity is not allowed
// here, i.e. it failed the configured e-mail domain allow-list. It is distinct
// from ErrOIDCLogin because it is worth telling the *operator* (in a log line)
// that someone they did not authorise knocked: the token was real.
var ErrOIDCAccount = errors.New("auth: this account is not allowed")

// OIDCConfig is one provider, as configured by the environment
// (OIDC_* variables; see internal/config).
type OIDCConfig struct {
	// Issuer is the provider's base URL, which is where the well-known
	// discovery document is read from. Endpoints are never configured by
	// hand: a provider that moves an endpoint should keep working after a
	// restart, and a hand-typed token endpoint is a way to send an
	// authorisation code somewhere else.
	Issuer string
	// ClientID and ClientSecret are this instance's registration at the
	// provider. ClientSecret is a credential: it is held here, never logged,
	// and never handed to the dashboard.
	ClientID     string
	ClientSecret string
	// RedirectURL is this instance's absolute callback URL, sent to the
	// provider as `redirect_uri` and registered there. It is configured
	// rather than derived from the request because redirect-URI matching is
	// the provider's line of defence, and a value that varies with the Host
	// header would let an attacker move the callback.
	RedirectURL string
	// Scopes is the scope list to request. Empty uses DefaultOIDCScopes.
	Scopes []string
	// Workspace is where a signed-in user lands, and the answer when no
	// claim-based mapping applies.
	Workspace workspace.ID
	// WorkspaceClaim, when set, names an ID-token claim whose value is used as
	// the workspace id instead of Workspace — the multi-tenant case: one
	// provider, one SoroBeacon, one workspace per group. The claim is trusted
	// because the provider signed the token that carries it.
	WorkspaceClaim string
	// AllowedDomains, when non-empty, is the allow-list an identity must pass:
	// its `email` claim must have one of these domains. Empty means every
	// account the provider will authenticate is accepted, which is a real
	// decision and documented as one.
	AllowedDomains []string
	// StateTTL bounds how long an unfinished login lives. Zero uses
	// DefaultLoginStateTTL.
	StateTTL time.Duration
}

// Identity is the verified caller an OIDC login produced. Verification already
// happened (signature, issuer, audience, expiry and nonce) by the time one of
// these exists, so its presence is the assertion, and nothing downstream
// re-checks a token.
type Identity struct {
	// Subject is the provider's stable local id for this user (`sub`). It is
	// the identity claim: e-mail addresses get reassigned and renamed, `sub`
	// does not, which is why the log line and any future per-user record key
	// off it rather off the address.
	Subject string
	Email   string
	Name    string
	// Workspace is where this caller acts, mapped per OIDCConfig above. It is
	// resolved here, at the one place that has both the claims and the
	// configuration, so no handler has to know how the mapping works.
	Workspace workspace.ID
}

// OIDCProvider is a configured provider: it can start a login and finish one.
//
// The zero value is not usable; build one with NewOIDCProvider, which does the
// discovery. A nil *OIDCProvider means "OIDC is off", which is what an instance
// without OIDC_ISSUER holds, and every method answers a nil receiver the way an
// unconfigured deployment behaves.
type OIDCProvider struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	// displayName is the issuer's host, for the sign-in page's button label.
	displayName string

	allowedDomains []string
	ws             workspace.ID
	wsClaim        string
	stateTTL       time.Duration

	// pending holds logins in flight, keyed by state. Server-side rather than
	// in a cookie because the values in it (nonce, PKCE verifier, next) must
	// not be attacker-chosen, and because a single-use value is easier to make
	// single-use when the place it lives can delete it. This matches the
	// dashboard's sessions, which are in memory too: see Authenticator.
	mu      sync.Mutex
	pending map[string]*pendingLogin
	now     func() time.Time
}

// pendingLogin is one login in flight.
type pendingLogin struct {
	nonce    string
	verifier string
	next     string
	expires  time.Time
}

// NewOIDCProvider discovers the provider and builds a login handler over it.
//
// Discovery happens at construction, not lazily on the first sign-in, so a
// mistyped issuer or an unreachable provider is a startup failure an operator
// sees in the deploy log rather than the reason a colleague cannot sign in
// three days later. The ctx given here bounds that request only.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCProvider, error) {
	issuer := strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if issuer == "" {
		return nil, errors.New("auth: OIDC issuer is required")
	}
	if err := validAbsoluteURL(issuer); err != nil {
		return nil, fmt.Errorf("auth: invalid OIDC issuer %q: %w", cfg.Issuer, err)
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("auth: OIDC client id is required")
	}
	if err := validAbsoluteURL(strings.TrimSpace(cfg.RedirectURL)); err != nil {
		return nil, fmt.Errorf("auth: invalid OIDC redirect URL %q: %w", cfg.RedirectURL, err)
	}
	ws := cfg.Workspace
	if ws == "" {
		ws = workspace.Default
	}
	if !workspace.Valid(string(ws)) {
		return nil, fmt.Errorf("auth: invalid OIDC workspace %q", ws)
	}
	ttl := cfg.StateTTL
	if ttl <= 0 {
		ttl = DefaultLoginStateTTL
	}

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: discover OIDC provider %s: %w", issuer, err)
	}
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  strings.TrimSpace(cfg.RedirectURL),
		Scopes:       normaliseScopes(cfg.Scopes),
		Endpoint:     provider.Endpoint(),
	}

	// The verifier is where signature, issuer, audience and expiry checks
	// live, so this package does not implement any of them. go-oidc fetches and
	// caches the provider's JWKS and rotates keys as the provider publishes
	// them, which is the part no hand-rolled verification gets right over time.
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})

	p := &OIDCProvider{
		oauth:          oauthCfg,
		verifier:       verifier,
		displayName:    issuerHost(issuer),
		allowedDomains: lowerAll(cfg.AllowedDomains),
		ws:             ws,
		wsClaim:        strings.TrimSpace(cfg.WorkspaceClaim),
		stateTTL:       ttl,
		pending:        map[string]*pendingLogin{},
		now:            time.Now,
	}
	return p, nil
}

func (p *OIDCProvider) withNow(f func() time.Time) *OIDCProvider {
	if f != nil {
		p.now = f
	}
	return p
}

// Enabled reports whether this is a usable provider. A nil receiver is the
// unconfigured case.
func (p *OIDCProvider) Enabled() bool { return p != nil && p.oauth != nil }

// DisplayName is the provider's issuer host, for the sign-in page's label.
// The issuer URL is configuration the operator chose, not user input, so it is
// safe to render — and it is a host, not a path an attacker controls.
func (p *OIDCProvider) DisplayName() string {
	if p == nil {
		return ""
	}
	return p.displayName
}

// BeginLogin starts a login and returns the provider URL to send the browser
// to, plus the state value to put in the browser's cookie. next is where to
// land afterwards and is kept server-side: read back from the state entry at
// the end, so a callback's own query cannot smuggle a redirect target through
// a login round-trip.
func (p *OIDCProvider) BeginLogin(next string) (redirectURL, state string, err error) {
	if !p.Enabled() {
		return "", "", errors.New("auth: OIDC is not configured")
	}
	state, err = oidcRandom()
	if err != nil {
		return "", "", err
	}
	nonce, err := oidcRandom()
	if err != nil {
		return "", "", err
	}
	verifier, err := oidcRandom()
	if err != nil {
		return "", "", err
	}
	now := p.now()
	p.mu.Lock()
	p.sweepLocked(now)
	p.pending[state] = &pendingLogin{
		nonce:    nonce,
		verifier: verifier,
		next:     next,
		expires:  now.Add(p.stateTTL),
	}
	p.mu.Unlock()

	// S256ChallengeOption adds code_challenge + code_challenge_method; the
	// matching VerifierOption on the exchange is what proves the party
	// redeeming the code is the party that started the flow.
	url := p.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	return url, state, nil
}

// StateCookie is the cookie pairing this browser with the login it started. Its
// lifetime is the login's: a state the server has already forgotten should not
// outlive it in a browser.
//
// SameSite must be Lax, not Strict: the provider finishes by navigating the
// browser back to us with a top-level GET, and Strict would withhold the
// cookie from exactly that request, so nobody could ever sign in.
func (p *OIDCProvider) StateCookie(state string, secure bool) *http.Cookie {
	ttl := DefaultLoginStateTTL
	if p != nil && p.stateTTL > 0 {
		ttl = p.stateTTL
	}
	return &http.Cookie{
		Name:     StateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(ttl.Seconds()),
	}
}

// ClearStateCookie expires the cookie, so a completed or abandoned login leaves
// no state behind for a later callback to reuse.
func (p *OIDCProvider) ClearStateCookie(secure bool) *http.Cookie {
	c := p.StateCookie("", secure)
	c.MaxAge = -1
	return c
}

// CompleteLogin finishes a login: it consumes the state (single use), exchanges
// the code, verifies the ID token, and applies the account and workspace rules.
// The second result is where the caller asked to land, read back from the
// consumed state rather than from this request's query.
//
// cookieState is what the browser presented, which may be "" (absent or
// expired). The raw query values are never logged, and neither is the code or
// any token: an authorisation code is a credential until it is spent, and the
// caller of this function writes the failure log line.
func (p *OIDCProvider) CompleteLogin(ctx context.Context, q url.Values, cookieState string) (*Identity, string, error) {
	if !p.Enabled() {
		return nil, "", ErrOIDCLogin
	}
	// A provider-reported failure (the user clicked "deny", consent was
	// withheld, the flow broke upstream) arrives on the same URL as a success.
	// It is reported separately because it is not a forgery symptom, and the
	// operator needs to tell the two apart from one log line.
	if e := q.Get("error"); e != "" {
		return nil, "", fmt.Errorf("auth: provider reported %s: %s", e, q.Get("error_description"))
	}

	state := q.Get("state")
	pending := p.take(state)
	if pending == nil {
		return nil, "", ErrOIDCLogin
	}
	// Constant-time, and only after the entry has been consumed: the state must
	// be both known to this process and held by this browser.
	if cookieState == "" || subtle.ConstantTimeCompare([]byte(cookieState), []byte(state)) != 1 {
		return nil, "", ErrOIDCLogin
	}
	code := q.Get("code")
	if code == "" {
		return nil, "", ErrOIDCLogin
	}

	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(pending.verifier))
	if err != nil {
		return nil, "", fmt.Errorf("auth: OIDC code exchange: %w", err)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || strings.TrimSpace(rawIDToken) == "" {
		return nil, "", errors.New("auth: OIDC token response carried no id_token")
	}
	// Verify is the signature, issuer, audience and expiry check: this package
	// does not implement any of them. go-oidc fetches and caches the provider's
	// JWKS and rotates keys as the provider publishes them, which is the part
	// no hand-rolled verification gets right over time.
	//
	// What it explicitly does *not* check is the nonce — the library documents
	// that as the caller's job — so the comparison below is the replay defence,
	// and it lives one line after the verify rather than somewhere the reader
	// has to look for.
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, "", fmt.Errorf("auth: OIDC id_token rejected: %w", err)
	}
	// Constant-time because it is a secret this process chose, compared against
	// a value an attacker can influence by choosing which token to present.
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(pending.nonce)) != 1 {
		return nil, "", fmt.Errorf("auth: OIDC id_token nonce mismatch: %w", ErrOIDCLogin)
	}

	var claims struct {
		Subject string `json:"sub"`
		Email   string `json:"email"`
		Name    string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, "", fmt.Errorf("auth: OIDC id_token claims: %w", err)
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return nil, "", errors.New("auth: OIDC id_token carried no sub claim")
	}

	identity := &Identity{
		Subject:   claims.Subject,
		Email:     claims.Email,
		Name:      claims.Name,
		Workspace: p.ws,
	}
	if err := p.checkAccount(identity); err != nil {
		return nil, "", err
	}
	if p.wsClaim != "" {
		// A second pass into the whole claim set: the typed struct above knows
		// the three claims this package cares about intrinsically, and the
		// workspace claim is a name the operator chose.
		var raw map[string]any
		if err := idToken.Claims(&raw); err != nil {
			return nil, "", fmt.Errorf("auth: OIDC id_token claims: %w", err)
		}
		if mapped, ok := claimString(raw, p.wsClaim); ok && workspace.Valid(mapped) {
			identity.Workspace = workspace.ID(mapped)
		}
		// An absent or unusable claim falls back to the configured workspace
		// rather than refusing the login: OIDC_WORKSPACE is the operator's own
		// statement of where such a user belongs, so the fallback is a choice
		// on record, not a silent default. It is logged by the caller, which
		// sees the workspace in the sign-in line it writes.
	}
	return identity, pending.next, nil
}

// take consumes a pending login, reporting nil for an unknown, expired or
// already-used state. Deleting on read is what makes a state single-use, so a
// captured callback URL cannot be replayed into a second session.
func (p *OIDCProvider) take(state string) *pendingLogin {
	if p == nil || state == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.pending[state]
	if !ok {
		return nil
	}
	delete(p.pending, state)
	if !p.now().Before(entry.expires) {
		return nil
	}
	return entry
}

// checkAccount applies the allow-list. With no domains configured every
// account the provider authenticates is accepted, which is the auto-provision
// behaviour: there is no user table to add someone to, so the first login is
// the provisioning event and the last is nobody's decision but the provider's.
func (p *OIDCProvider) checkAccount(id *Identity) error {
	if len(p.allowedDomains) == 0 {
		return nil
	}
	domain := ""
	if _, after, found := strings.Cut(strings.ToLower(id.Email), "@"); found {
		domain = after
	}
	if domain == "" {
		return fmt.Errorf("%w: no email claim to match against the allowed domains", ErrOIDCAccount)
	}
	if !contains(p.allowedDomains, domain) {
		return fmt.Errorf("%w: %s is not in an allowed domain", ErrOIDCAccount, domain)
	}
	return nil
}

func (p *OIDCProvider) sweepLocked(now time.Time) {
	for state, entry := range p.pending {
		if !now.Before(entry.expires) {
			delete(p.pending, state)
		}
	}
}

// validAbsoluteURL checks a configured URL is absolute http(s) with a host.
func validAbsoluteURL(v string) error {
	if v == "" {
		return errors.New("must be an absolute http or https URL")
	}
	u, err := url.Parse(v)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be an absolute http or https URL")
	}
	if u.Host == "" {
		return errors.New("must name a host")
	}
	return nil
}

// normaliseScopes guarantees `openid` and drops blanks and duplicates: without
// it, a config that says "email profile" produces a provider response with no
// ID token, and the failure surfaces at sign-in rather than at startup.
func normaliseScopes(in []string) []string {
	out := make([]string, 0, len(in)+1)
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range in {
		for _, part := range strings.Fields(s) {
			add(part)
		}
	}
	if !seen["openid"] {
		out = append([]string{"openid"}, out...)
	}
	return out
}

// claimString reads a top-level claim as a string. A workspace claim is only
// meaningful as one, and a provider that answers with a number or an object is
// treated as not having supplied it rather than coerced.
func claimString(claims map[string]any, name string) (string, bool) {
	v, ok := claims[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

func lowerAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// contains reports an exact match; both sides are already lowered by lowerAll.
func contains(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// oidcRandom is a URL-safe single-use value: state, nonce or PKCE verifier.
// base64url of 32 bytes is 43 characters, inside the 43–128 range RFC 7636
// allows a code_verifier to have, so one function serves all three.
func oidcRandom() (string, error) {
	buf := make([]byte, oidcRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("auth: OIDC login: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// issuerHost is the label for the sign-in button: an operator recognising
// which provider they configured, and a user recognising who is about to be
// told who they are signing in to.
func issuerHost(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return issuer
	}
	return u.Hostname()
}
