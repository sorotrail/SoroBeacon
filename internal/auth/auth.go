// Package auth holds SoroBeacon's credential primitives: the static bearer
// tokens configured through API_TOKEN (and WORKSPACE_TOKENS, which pairs a
// token with a workspace), and the in-memory dashboard sessions minted from
// one of them.
//
// It is shared by the JSON API (internal/api) and the dashboard
// (internal/web) so both check the same tokens and the same session cookie;
// neither package owns the other. With no tokens configured everything is
// open, which is how a deployment that never set API_TOKEN keeps working.
//
// Credentials are also the tenancy boundary: the workspace a request acts on
// is read from the credential that authenticated it (Workspace), never from a
// header or query parameter, so a caller cannot choose its own tenant.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const (
	// SessionCookie is the dashboard's session cookie name. One constant so
	// the package that sets it (internal/web) and the package that reads it
	// (internal/api) cannot drift apart.
	SessionCookie = "sorobeacon_session"

	// DefaultSessionTTL is how long a dashboard session stays valid. Short
	// enough that a stolen cookie ages out on its own, long enough that an
	// operator does not re-enter the token in the middle of an incident.
	DefaultSessionTTL = 12 * time.Hour

	// bearerPrefix is the RFC 7235 auth-scheme, matched case-insensitively.
	bearerPrefix = "bearer "

	// sessionIDBytes is the raw entropy per session id. 256 bits is the
	// standard guess-resistance budget for a bearer credential.
	sessionIDBytes = 32
)

// Authenticator verifies bearer tokens and keeps dashboard sessions.
//
// The zero value is not usable; build one with New. A nil *Authenticator is
// treated as "no authentication configured" by the middlewares that accept
// it, so a server built without WithAuth keeps its previous open behaviour.
type Authenticator struct {
	// tokens are the configured credentials, each with the workspace it
	// selects. Tokens are hashed once at construction so comparisons use
	// fixed-width values: crypto/subtle.ConstantTimeCompare returns
	// immediately on a length mismatch, which would otherwise leak each
	// configured token's length.
	tokens []credential

	// dbTokens, when set, is the store of scoped, revocable tokens. It is nil
	// for a deployment that only configured API_TOKEN / WORKSPACE_TOKENS, and
	// a nil manager is simply skipped: the static and session paths below are
	// unchanged by its absence.
	dbTokens *Manager

	// oidc, when set, is a provider that may sign users in to the dashboard.
	// Like dbTokens it is nil for a deployment that did not configure it, and
	// its only effect on the paths below is which credentials are accepted at
	// the sign-in page — once a session exists, nothing can tell how it began.
	oidc *OIDCProvider

	ttl      time.Duration
	mu       sync.Mutex
	sessions map[string]session
	now      func() time.Time
}

// credential is one accepted token and the workspace it belongs to. The
// workspace rides with the digest so a check can both authenticate and route
// without a second comparison.
type credential struct {
	ws     workspace.ID
	digest [sha256.Size]byte
}

// session is one live dashboard sign-in. It remembers the workspace the token
// that minted it belonged to, so the browser cannot widen its own scope after
// sign-in: the cookie is only ever as powerful as the credential behind it.
type session struct {
	expires time.Time
	ws      workspace.ID
}

// New builds an Authenticator over the configured tokens. A ttl <= 0 uses
// DefaultSessionTTL. Empty or blank entries are dropped; callers pass
// already-validated tokens (internal/config rejects an API_TOKEN that
// contains nothing usable).
//
// Every token here selects the default workspace, which is the whole of
// tenancy for a single-tenant deployment. Use NewBound to configure one
// workspace per token (WORKSPACE_TOKENS).
func New(tokens []string, ttl time.Duration) *Authenticator {
	bindings := make([]Binding, 0, len(tokens))
	for _, t := range tokens {
		bindings = append(bindings, Binding{Workspace: workspace.Default, Token: t})
	}
	return NewBound(bindings, ttl)
}

// Binding is one token paired with the workspace it grants access to. It is
// the input NewBound takes; internal/config parses WORKSPACE_TOKENS into
// these so validation and secret-holding stay in one place.
type Binding struct {
	Workspace workspace.ID
	Token     string
}

// NewBound builds an Authenticator from workspace-bound tokens. A ttl <= 0
// uses DefaultSessionTTL. Blank tokens and bindings for an invalid workspace
// id are dropped rather than downgraded to the default workspace: silently
// merging a misconfigured tenant into `default` would hand it that tenant's
// data, whereas dropping it leaves it with no access at all — which fails
// loudly at the first request. internal/config rejects both cases at startup,
// so dropping is a safety net rather than the normal path.
func NewBound(bindings []Binding, ttl time.Duration) *Authenticator {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	a := &Authenticator{
		ttl:      ttl,
		sessions: make(map[string]session),
		now:      time.Now,
	}
	for _, b := range bindings {
		t := strings.TrimSpace(b.Token)
		if t == "" || !workspace.Valid(string(b.Workspace)) {
			continue
		}
		a.tokens = append(a.tokens, credential{
			ws:     b.Workspace,
			digest: sha256.Sum256([]byte(t)),
		})
	}
	return a
}

// SessionTTL is how long a session minted here stays valid. The dashboard
// reads it so the cookie it sets and the server-side session expire
// together instead of drifting apart.
func (a *Authenticator) SessionTTL() time.Duration {
	if a == nil || a.ttl <= 0 {
		return DefaultSessionTTL
	}
	return a.ttl
}

// Enabled reports whether a credential is required. When it is not, both the
// API and the dashboard stay open: the middlewares step aside entirely and the
// process logs one warning at startup instead of failing, so an upgrade or a
// docker-compose quickstart never locks the operator out.
//
// An OIDC provider counts as a credential source on its own: a deployment that
// configured SSO but no API_TOKEN has said "sign in to reach this", and the
// dashboard must not be open to whoever finds the port while that is being
// arranged.
func (a *Authenticator) Enabled() bool {
	return a != nil && (len(a.tokens) > 0 || a.oidc.Enabled())
}

// WithTokens teaches this authenticator the database-backed credentials. It is
// a setter rather than a constructor argument because the manager needs the
// store, and the store is built before the authenticator is; it returns the
// authenticator so main's wire-up reads as one statement.
//
// A nil manager leaves the behaviour exactly as it was: static tokens and
// sessions, every one of them unrestricted.
func (a *Authenticator) WithTokens(m *Manager) *Authenticator {
	if a != nil {
		a.dbTokens = m
	}
	return a
}

// Tokens returns the database-token manager, nil when this deployment has
// none. The API and dashboard mint, list and revoke through it so the rules
// about what a token may hold live in one place.
func (a *Authenticator) Tokens() *Manager {
	if a == nil {
		return nil
	}
	return a.dbTokens
}

// HasStaticTokens reports whether the token form at /login can succeed, i.e. at
// least one API_TOKEN / WORKSPACE_TOKENS credential is configured. It is what
// lets the sign-in page hide a form that could only ever answer "not accepted"
// — the state an SSO-only deployment is in — without the page having to know
// anything about how tokens are verified.
func (a *Authenticator) HasStaticTokens() bool {
	return a != nil && len(a.tokens) > 0
}

// WithOIDC teaches this authenticator an OpenID Connect provider, the way
// WithTokens teaches it the token table: the provider needs a context to do
// discovery in, so it is built after the authenticator and cannot be a
// constructor argument.
func (a *Authenticator) WithOIDC(p *OIDCProvider) *Authenticator {
	if a != nil {
		a.oidc = p
	}
	return a
}

// OIDC returns the configured provider, nil when sign-in is token-only. The
// dashboard reads it to decide whether to offer the SSO button.
func (a *Authenticator) OIDC() *OIDCProvider {
	if a == nil {
		return nil
	}
	return a.oidc
}

// LoginOIDC mints the dashboard session a verified OIDC identity is given. It
// is Login's counterpart for the other half of the sign-in page, and it takes
// an *Identity rather than a string so no caller can hand it a subject the
// provider did not assert: the verification happened in the provider, and this
// is the only door from there to a session.
func (a *Authenticator) LoginOIDC(id *Identity) string {
	if a == nil || id == nil {
		return ""
	}
	return a.NewSession(id.Workspace)
}

// Verify reports whether candidate matches any configured token.
//
// Every configured token is compared — the loop accumulates the result
// instead of returning early — so the time taken does not reveal how many
// tokens exist or which one matched. Accepting a list at all is what makes
// rotation work: add the new token, roll clients over, remove the old one.
func (a *Authenticator) Verify(candidate string) bool {
	_, ok := a.VerifyWorkspace(candidate)
	return ok
}

// VerifyWorkspace is Verify with the bound workspace: it reports which
// workspace the matching token selects. A false result carries no workspace,
// and a token that is configured twice for two workspaces is a
// misconfiguration internal/config rejects at startup — resolution stays one
// way.
func (a *Authenticator) VerifyWorkspace(candidate string) (workspace.ID, bool) {
	if !a.Enabled() {
		return "", false
	}
	got := sha256.Sum256([]byte(candidate))
	var ws workspace.ID
	matched := false
	for _, c := range a.tokens {
		// The comparison is constant-time and the loop never returns early,
		// so a non-matching token costs the same as a matching one; picking
		// up the winner's workspace is a plain field copy.
		if subtle.ConstantTimeCompare(got[:], c.digest[:]) == 1 {
			matched = true
			ws = c.ws
		}
	}
	if !matched {
		return "", false
	}
	return ws, true
}

// Bearer extracts the token from an Authorization header value. The scheme
// is matched case-insensitively (RFC 7235) and surrounding whitespace is
// trimmed, so "bearer abc", "Bearer abc" and "Bearer  abc " all work.
// Anything else — a missing scheme, another scheme such as Basic, or an
// empty token — reports false.
func Bearer(header string) (string, bool) {
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// Login verifies a token submitted to the dashboard's sign-in form and, on
// success, mints a session id for the caller to put in SessionCookie. The
// id is returned only to the caller: the submitted token is never echoed
// back, logged, or stored.
//
// The session inherits the workspace of the token that signed in, which is
// what makes tenancy stick to a browser session: after sign-in there is no
// other way to say which workspace a request is for.
//
// Only a static token can sign in. A scoped, database-backed token cannot, and
// that is the point: a session carries no scope list, so logging in with a CI
// credential would trade a token that can be revoked for a cookie that can
// only be killed by a restart — and a browser is the worst place to hold a
// least-privilege secret.
func (a *Authenticator) Login(token string) (string, bool) {
	ws, ok := a.VerifyWorkspace(token)
	if !ok {
		return "", false
	}
	id := a.NewSession(ws)
	if id == "" {
		// Minting failed (no system entropy, or a workspace id that cannot
		// name a workspace). Report "not accepted" rather than returning an
		// id with no server-side session behind it: the dashboard would set a
		// cookie that bounce-redirects the caller back to sign-in forever.
		return "", false
	}
	return id, true
}

// NewSession mints a session id bound to ws. It exists for the login handler;
// callers that already hold a live session should use HasSession instead.
func (a *Authenticator) NewSession(ws workspace.ID) string {
	if !workspace.Valid(string(ws)) {
		return ""
	}
	buf := make([]byte, sessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is unrecoverable for a credential: returning
		// a predictable id would be worse than refusing to sign anyone in.
		return ""
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.sweepLocked(now)
	a.sessions[id] = session{expires: now.Add(a.ttl), ws: ws}
	return id
}

// HasSession reports whether id names a live, unexpired session. Sessions
// live in memory only: a restart signs everyone out, which is the honest
// behaviour for a single static credential — there is no user database to
// invalidate against, and a session cookie is worth exactly one token.
func (a *Authenticator) HasSession(id string) bool {
	_, ok := a.SessionWorkspace(id)
	return ok
}

// SessionWorkspace returns the workspace a live session was signed into, or
// ok false for an unknown or expired id.
func (a *Authenticator) SessionWorkspace(id string) (workspace.ID, bool) {
	if a == nil || id == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		return "", false
	}
	if !a.now().Before(s.expires) {
		delete(a.sessions, id)
		return "", false
	}
	return s.ws, true
}

// DropSession ends a session (the dashboard's sign-out button). Unknown ids
// are ignored so signing out twice is not an error.
func (a *Authenticator) DropSession(id string) {
	if a == nil || id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, id)
}

func (a *Authenticator) sweepLocked(now time.Time) {
	for id, s := range a.sessions {
		if !now.Before(s.expires) {
			delete(a.sessions, id)
		}
	}
}

// SessionID reads the dashboard session id from r's cookies, or "" when the
// cookie is absent.
func SessionID(r *http.Request) string {
	if r == nil {
		return ""
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// Workspace resolves the workspace a request's credential belongs to. It is
// Principal reduced to the tenancy question, and every request that needs the
// scopes too (the API middleware) calls Principal instead.
func (a *Authenticator) Workspace(r *http.Request) (workspace.ID, bool) {
	p, ok := a.Principal(r)
	if !ok {
		return "", false
	}
	return p.Workspace, true
}

// Principal is the only supported way to turn an HTTP request into a caller, and
// it reads the credential alone: not a header, not a query parameter, not the
// host. A client that could name its own workspace could read someone else's, so
// there is no X-Workspace header and there must not be one.
//
// A request is authenticated exactly when this reports ok: it carries either
// an Authorization: Bearer token (scripts, CI, other services) or a live
// dashboard session cookie (a browser that signed in, including the
// dashboard's own same-origin calls such as the CSV export link, which cannot
// attach a header).
//
// With no token configured, every request is authenticated, belongs to the
// default workspace and carries no scope list, which is how a single-tenant
// deployment — and the pre-tenancy behaviour of an unset API_TOKEN — keeps
// working unchanged. An unknown, revoked or expired credential yields ok false,
// and the caller's middleware rejects the request rather than falling back to a
// workspace: a bad credential must never become a read of someone's data.
//
// The bearer token is tried first because it is the more specific statement:
// a request carrying both a token and a session cookie came from something
// deliberately presenting the token, not from a stale browser tab.
//
// A bearer string is then tried in one order and one order only. A token shaped
// like a database token (TokenPrefix) is offered to the token store and nothing
// else; any other bearer is compared against the static tokens. Falling through
// from a rejected scoped token to the static list would hand a caller a second
// guess against a different credential, and would let a revoked token keep
// working if its owner ever also configured it as API_TOKEN.
//
// A dashboard session is an unrestricted principal: it was minted by a static
// token or by a verified OIDC identity, and the scopes of the credential that
// signed in are not a thing a cookie can carry forward (see Login).
func (a *Authenticator) Principal(r *http.Request) (*Principal, bool) {
	if !a.Enabled() {
		return &Principal{Workspace: workspace.Default}, true
	}
	if token, ok := Bearer(r.Header.Get("Authorization")); ok {
		if a.dbTokens != nil && IsTokenBearer(token) {
			p, err := a.dbTokens.Authenticate(r.Context(), token)
			if err != nil {
				return nil, false
			}
			return p, true
		}
		if ws, matched := a.VerifyWorkspace(token); matched {
			return &Principal{Workspace: ws}, true
		}
	}
	// Session ids are 256 bits of crypto/rand and looked up by exact match,
	// so a map lookup is not a timing oracle here (unlike the token compare,
	// where the attacker supplies guesses against a low-entropy secret).
	if ws, ok := a.SessionWorkspace(SessionID(r)); ok {
		return &Principal{Workspace: ws}, true
	}
	return nil, false
}
