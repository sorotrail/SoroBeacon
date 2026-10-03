package web

// The dashboard's OpenID Connect round-trip: two GET routes, one to send the
// browser to the provider and one to receive it back.
//
// Both are exempt from the session gate, and the exemption is the whole point
// of them: a visitor who is not signed in is exactly who needs the sign-in
// flow. Everything after the callback is the ordinary session path — the
// dashboard cannot tell a signed-in-through-SSO request from a
// signed-in-with-a-token one, and neither should it have to.
//
// The state value is carried twice on purpose: the server keeps the nonce, the
// PKCE verifier and the post-login target under it, and the browser gets a
// cookie holding it. The provider only ever sees one copy of the value, and the
// callback has to present the same one in both places to be believed.

import (
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

const (
	// oidcStartPath begins a login; oidcCallbackPath is the provider's
	// redirect target (OIDC_REDIRECT_URL must name it, e.g.
	// https://beacon.example.com/login/oidc/callback).
	oidcStartPath    = "/login/oidc"
	oidcCallbackPath = "/login/oidc/callback"
)

// oidcStart sends the browser off to the provider.
//
// next is filtered through safeNext here and then travels *inside the state
// entry*, not through the provider: the callback reads it back from the entry it
// consumes, so a login round-trip cannot be used to launder a redirect to
// somewhere else — the value that comes out is the value that went in.
func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	p := s.auth.OIDC()
	if !p.Enabled() {
		// Not configured is not an error the visitor can act on; it is this
		// instance having no SSO, which the sign-in page already reflects by
		// not offering the button.
		http.NotFound(w, r)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	redirect, state, err := p.BeginLogin(next)
	if err != nil {
		s.log.Error("oidc login start failed", "err", err, "remote_addr", r.RemoteAddr)
		s.renderStatus(w, r, http.StatusBadGateway, "login", s.loginData(r, next,
			"Sign-in could not be started. Check the provider's configuration."))
		return
	}
	http.SetCookie(w, p.StateCookie(state, r.TLS != nil))
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// oidcCallback completes the login the browser started.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	p := s.auth.OIDC()
	if !p.Enabled() {
		http.NotFound(w, r)
		return
	}
	state := ""
	if c, err := r.Cookie(auth.StateCookie); err == nil {
		state = c.Value
	}
	identity, next, err := p.CompleteLogin(r.Context(), r.URL.Query(), state)
	// The state is spent either way: it is single-use, and leaving the cookie
	// behind would give a later callback a value to be replayed with.
	http.SetCookie(w, p.ClearStateCookie(r.TLS != nil))
	if err != nil {
		// The log carries the reason and the browser carries a generic line. The
		// distinction is not a secret from the visitor (they caused it) but it is
		// a secret from anyone watching the callback URL, and the details of why
		// verification failed tell an attacker nothing they can use.
		s.log.Warn("oidc sign-in rejected", "err", err, "remote_addr", r.RemoteAddr)
		s.renderStatus(w, r, http.StatusUnauthorized, "login", s.loginData(r, next,
			"Sign-in did not complete. Please try again."))
		return
	}
	sessionID := s.auth.LoginOIDC(identity)
	if sessionID == "" {
		s.log.Error("oidc sign-in could not start a session", "remote_addr", r.RemoteAddr)
		s.renderStatus(w, r, http.StatusInternalServerError, "login", s.loginData(r, next,
			"Sign-in could not start a session."))
		return
	}

	http.SetCookie(w, sessionCookie(sessionID, r.TLS != nil, s.auth.SessionTTL()))
	// The subject is what identifies who signed in; the e-mail is deliberately
	// left out, since a log line is not where an address belongs.
	s.log.Info("dashboard signed in", "via", "oidc", "oidc_subject", identity.Subject,
		"workspace", identity.Workspace)
	http.Redirect(w, r, next, http.StatusSeeOther)
}
