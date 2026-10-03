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
type tokenEntry struct {
	digest [sha256.Size]byte
	role   Role
	// workspace is the tenant this token grants access to. It is the whole
	// point of a Binding: the credential names the workspace, so a request
	// never can.
	workspace workspace.ID
}

type sessionInfo struct {
	expires time.Time
	role    Role
	// workspace is inherited from the token that signed in. After sign-in
	// there is nothing else that could say which tenant a request is for.
	workspace workspace.ID
}

type Authenticator struct {
	// digests are SHA-256 hashes of the configured tokens. Tokens are
	// hashed once at construction so Verify compares fixed-width values:
	// crypto/subtle.ConstantTimeCompare returns immediately on a length
	// mismatch, which would otherwise leak each configured token's length.
	tokens []tokenEntry

	// scoped resolves database-backed tokens (TokenPrefix). It is optional:
	// without it only static tokens and sessions authenticate, which is how
	// a deployment that never created an API token keeps working.
	scoped *Manager

	// oidc is the single-sign-on provider, when one is configured. Nil leaves
	// the dashboard on its token form, which is every deployment that has not
	// set OIDC_ISSUER.
	oidc *OIDCProvider

	tt       time.Duration
	mu       sync.Mutex
	sessions map[string]sessionInfo
	now      func() time.Time
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
		tt:       ttl,
		sessions: make(map[string]sessionInfo),
		now:      time.Now,
	}
	for _, b := range bindings {
		t := strings.TrimSpace(b.Token)
		if t == "" || !workspace.Valid(string(b.Workspace)) {
			continue
		}
		// Support explicit role suffix if present e.g. token:admin, otherwise default to admin for backwards compatibility
		role := RoleAdmin
		if idx := strings.LastIndex(t, ":"); idx != -1 {
			if r, ok := ParseRole(t[idx+1:]); ok {
				role = r
				t = t[:idx]
			}
		}
		a.tokens = append(a.tokens, tokenEntry{
			digest:    sha256.Sum256([]byte(t)),
			role:      role,
			workspace: b.Workspace,
		})
	}
	return a
}

// SessionTTL is how long a session minted here stays valid. The dashboard
// reads it so the cookie it sets and the server-side session expire
// together instead of drifting apart.
func (a *Authenticator) SessionTTL() time.Duration {
	if a == nil || a.tt <= 0 {
		return DefaultSessionTTL
	}
	return a.tt
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
	// Either credential source closes the door. An SSO-only deployment has no
	// static token and is still protected, so reporting disabled would leave
	// every page open to whoever finds the port.
	return a != nil && (len(a.tokens) > 0 || a.oidc.Enabled())
}

// Verify reports whether candidate matches any configured token.
//
// Every configured token is compared — the loop accumulates the result
// instead of returning early — so the time taken does not reveal how many
// tokens exist or which one matched. Accepting a list at all is what makes
// rotation work: add the new token, roll clients over, remove the old one.
func (a *Authenticator) Verify(candidate string) bool {
	_, ok := a.verifyToken(candidate)
	return ok
}

func (a *Authenticator) verifyToken(candidate string) (Role, bool) {
	entry, ok := a.lookupStatic(candidate)
	if !ok {
		if !a.Enabled() {
			return RoleAdmin, false
		}
		return RoleUnknown, false
	}
	return entry.role, true
}

// lookupStatic resolves a candidate against the configured tokens, returning
// the entry it matched so the caller gets the workspace as well as the role.
//
// A candidate carrying TokenPrefix is refused outright, before the table is
// consulted: that prefix belongs to database-backed tokens, which are revoked
// by deleting a row. Letting one match here would mean a revoked or unknown
// scoped token could be revived by also configuring it as API_TOKEN, and the
// revocation an operator performed would silently stop meaning anything.
func (a *Authenticator) lookupStatic(candidate string) (tokenEntry, bool) {
	if !a.HasStaticTokens() || strings.HasPrefix(candidate, TokenPrefix) {
		return tokenEntry{}, false
	}
	// Every configured token is compared — the loop accumulates the result
	// instead of returning early — so the time taken does not reveal how many
	// tokens exist or which one matched.
	got := sha256.Sum256([]byte(candidate))
	matched := tokenEntry{role: RoleViewer, workspace: workspace.Default}
	match := 0
	for _, entry := range a.tokens {
		m := subtle.ConstantTimeCompare(got[:], entry.digest[:])
		if m == 1 {
			matched = entry
		}
		match |= m
	}
	if match == 1 {
		return matched, true
	}
	return tokenEntry{}, false
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
	entry, ok := a.lookupStatic(token)
	if !ok {
		return "", false
	}
	return a.newSessionFor(entry.role, entry.workspace), true
}

// NewSessionWithRole mints a session id associated with a specific role. The
// session belongs to the default workspace; Login is the path that carries a
// token's own workspace into the session.
func (a *Authenticator) NewSessionWithRole(role Role) string {
	return a.newSessionFor(role, workspace.Default)
}

func (a *Authenticator) newSessionFor(role Role, ws workspace.ID) string {
	if ws == "" {
		ws = workspace.Default
	}
	buf := make([]byte, sessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(a.now())
	a.sessions[id] = sessionInfo{
		expires:   a.now().Add(a.tt),
		role:      role,
		workspace: ws,
	}
	return id
}

// NewSession mints a session id in the named workspace. It exists for the
// login handler; callers that already hold a live session should use
// HasSession instead. The workspace is explicit because a session is a tenancy
// decision: there is no later point at which one could be inferred.
func (a *Authenticator) NewSession(ws workspace.ID) string {
	return a.newSessionFor(RoleAdmin, ws)
}

// HasSession reports whether id names a live, unexpired session. Sessions
// live in memory only: a restart signs everyone out, which is the honest
// behaviour for a single static credential — there is no user database to
// invalidate against, and a session cookie is worth exactly one token.
func (a *Authenticator) HasSession(id string) bool {
	_, ok := a.getSessionRole(id)
	return ok
}

func (a *Authenticator) getSessionRole(id string) (Role, bool) {
	if a == nil || id == "" {
		return RoleUnknown, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	info, ok := a.sessions[id]
	if !ok {
		return RoleUnknown, false
	}
	if !a.now().Before(info.expires) {
		delete(a.sessions, id)
		return RoleUnknown, false
	}
	return info.role, true
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
	for id, info := range a.sessions {
		if !now.Before(info.expires) {
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

// getSession returns a live session's record. It is getSessionRole plus the
// workspace, which Principal needs and HasSession does not.
func (a *Authenticator) getSession(id string) (sessionInfo, bool) {
	if a == nil || id == "" {
		return sessionInfo{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	info, ok := a.sessions[id]
	if !ok {
		return sessionInfo{}, false
	}
	if !a.now().Before(info.expires) {
		delete(a.sessions, id)
		return sessionInfo{}, false
	}
	return info, true
}

// WithTokens attaches the database-backed token manager, which is what makes
// scoped tokens (TokenPrefix) authenticate. Without it only static tokens and
// dashboard sessions do, which is the behaviour of a deployment that has never
// minted an API token.
func (a *Authenticator) WithTokens(m *Manager) *Authenticator {
	if a == nil {
		return a
	}
	a.scoped = m
	return a
}

// WithOIDC attaches the single-sign-on provider. Nil is the no-SSO case and
// leaves the sign-in page offering the token form alone.
func (a *Authenticator) WithOIDC(p *OIDCProvider) *Authenticator {
	if a == nil {
		return a
	}
	a.oidc = p
	return a
}

// OIDC returns the configured provider, which may be nil. Callers test it with
// Enabled() rather than against nil, so a dashboard that has no SSO and one
// that was never given an authenticator take the same path.
func (a *Authenticator) OIDC() *OIDCProvider {
	if a == nil {
		return nil
	}
	return a.oidc
}

// HasStaticTokens reports whether a credential can be typed into the sign-in
// form. It is deliberately not Enabled(): with only SSO configured the
// dashboard is still protected, and offering a token box that nothing can
// satisfy would read as a broken page.
func (a *Authenticator) HasStaticTokens() bool {
	return a != nil && len(a.tokens) > 0
}

// SessionWorkspace resolves a session cookie to the workspace it signed in to.
// It is the dashboard's whole authentication check: a live session is the
// credential, and the workspace it carries is the tenant every page then reads.
func (a *Authenticator) SessionWorkspace(id string) (workspace.ID, bool) {
	if a == nil {
		return "", false
	}
	if !a.Enabled() {
		// Nothing configured: the dashboard is open, as it has always been,
		// and serves the single workspace everything already lives in.
		return workspace.Default, true
	}
	info, ok := a.getSession(id)
	if !ok {
		return "", false
	}
	return info.workspace, true
}

// LoginOIDC mints a dashboard session for an identity the provider verified.
// The session is unrestricted, like one minted from a static token: a cookie
// carries no scope list, and the workspace comes from the identity's mapping
// rather than from anything the browser sent.
func (a *Authenticator) LoginOIDC(id *Identity) string {
	if a == nil || id == nil {
		return ""
	}
	return a.newSessionFor(RoleAdmin, id.Workspace)
}

// Principal resolves a request to its caller. It is the one place that turns a
// credential into a tenant, and it reads the credential alone — not a header,
// not a query parameter, not the host — because a client that could name its
// own workspace could read someone else's.
//
// The three credentials, in the order they are tried:
//
//   - A bearer token carrying TokenPrefix is a database-backed token: the
//     manager resolves it to a restricted principal whose scopes are the whole
//     grant. It is never compared against the static table, so revoking the row
//     revokes the credential.
//   - Any other bearer token is matched against the configured static tokens,
//     and yields an unrestricted principal in that token's workspace.
//   - Failing both, a live dashboard session cookie yields an unrestricted
//     principal in the workspace of the token that signed in. The dashboard's
//     own same-origin requests (the CSV export link, for one) cannot attach a
//     header, which is why the cookie is a credential here at all.
//
// With no tokens configured the service is open, as it has always been, and
// every request acts on the default workspace.
func (a *Authenticator) Principal(r *http.Request) (*Principal, bool) {
	if r == nil {
		return nil, false
	}
	// A nil authenticator is a server built without WithAuth, which is the
	// same "nothing configured" case as an empty token list.
	if !a.Enabled() {
		return &Principal{Workspace: workspace.Default}, true
	}
	if raw, ok := Bearer(r.Header.Get("Authorization")); ok {
		if strings.HasPrefix(raw, TokenPrefix) {
			if a.scoped == nil {
				return nil, false
			}
			p, err := a.scoped.Authenticate(r.Context(), raw)
			if err != nil {
				return nil, false
			}
			return p, true
		}
		if entry, ok := a.lookupStatic(raw); ok {
			return &Principal{Workspace: entry.workspace}, true
		}
		// A bearer header that named a credential we do not know is a failed
		// attempt, not an anonymous request: falling through to the cookie
		// would let a stale token ride a live browser session.
		return nil, false
	}
	if info, ok := a.getSession(SessionID(r)); ok {
		return &Principal{Workspace: info.workspace}, true
	}
	return nil, false
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
// dashboard's own same-origin calls such as the CSV export link, which
// cannot attach a header).
func (a *Authenticator) Authenticated(r *http.Request) bool {
	role, ok := a.RoleForRequest(r)
	return ok && role.HasPermission(RoleViewer)
}

// RoleForRequest determines the effective Role for an HTTP request.
func (a *Authenticator) RoleForRequest(r *http.Request) (Role, bool) {
	if !a.Enabled() {
		return RoleAdmin, true
	}
	if token, ok := Bearer(r.Header.Get("Authorization")); ok {
		if role, ok := a.verifyToken(token); ok {
			return role, true
		}
	}
	if role, ok := a.getSessionRole(SessionID(r)); ok {
		return role, true
	}
	return RoleUnknown, false
}
