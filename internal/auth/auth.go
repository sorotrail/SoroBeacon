// Package auth holds SoroBeacon's credential primitives: the static bearer
// tokens configured through API_TOKEN, and the in-memory dashboard sessions
// minted from one of them.
//
// It is shared by the JSON API (internal/api) and the dashboard
// (internal/web) so both check the same tokens and the same session cookie;
// neither package owns the other. With no tokens configured everything is
// open, which is how a deployment that never set API_TOKEN keeps working.
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
}

type sessionInfo struct {
	expires time.Time
	role    Role
}

type Authenticator struct {
	// digests are SHA-256 hashes of the configured tokens. Tokens are
	// hashed once at construction so Verify compares fixed-width values:
	// crypto/subtle.ConstantTimeCompare returns immediately on a length
	// mismatch, which would otherwise leak each configured token's length.
	tokens []tokenEntry

	tt       time.Duration
	mu       sync.Mutex
	sessions map[string]sessionInfo
	now      func() time.Time
}

// New builds an Authenticator over the configured tokens. A ttl <= 0 uses
// DefaultSessionTTL. Empty or blank entries are dropped; callers pass
// already-validated tokens (internal/config rejects an API_TOKEN that
// contains nothing usable).
func New(tokens []string, ttl time.Duration) *Authenticator {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	a := &Authenticator{
		tt:       ttl,
		sessions: make(map[string]sessionInfo),
		now:      time.Now,
	}
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
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
			digest: sha256.Sum256([]byte(t)),
			role:   role,
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

// Enabled reports whether a token is configured. When it is not, both the
// API and the dashboard stay open: the middlewares step aside entirely and
// the process logs one warning at startup instead of failing, so an upgrade
// or a docker-compose quickstart never locks the operator out.
func (a *Authenticator) Enabled() bool {
	return a != nil && len(a.tokens) > 0
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
	if !a.Enabled() {
		return RoleAdmin, false
	}
	got := sha256.Sum256([]byte(candidate))
	matchedRole := RoleViewer
	match := 0
	for _, entry := range a.tokens {
		m := subtle.ConstantTimeCompare(got[:], entry.digest[:])
		if m == 1 {
			matchedRole = entry.role
		}
		match |= m
	}
	if match == 1 {
		return matchedRole, true
	}
	return RoleUnknown, false
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
func (a *Authenticator) Login(token string) (string, bool) {
	role, ok := a.verifyToken(token)
	if !ok {
		return "", false
	}
	return a.NewSessionWithRole(role), true
}

// NewSessionWithRole mints a session id associated with a specific role.
func (a *Authenticator) NewSessionWithRole(role Role) string {
	buf := make([]byte, sessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(a.now())
	a.sessions[id] = sessionInfo{
		expires: a.now().Add(a.tt),
		role:    role,
	}
	return id
}

// NewSession mints a session id. It exists for the login handler; callers
// that already hold a live session should use HasSession instead.
func (a *Authenticator) NewSession() string {
	return a.NewSessionWithRole(RoleAdmin)
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

// Authenticated reports whether r carries a valid credential, either an
// Authorization: Bearer token (scripts, CI, other services) or a live
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
