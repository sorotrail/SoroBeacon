package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// Dashboard sign-in.
//
// The dashboard has no user accounts and is not going to grow any: it accepts
// the same static token as the API (API_TOKEN, or one workspace's token from
// WORKSPACE_TOKENS) and, on success, sets an HttpOnly session cookie. A cookie
// rather than HTTP Basic because Basic credentials are cached by the browser
// and replayed on requests the page did not initiate — including cross-site
// ones — while a SameSite=Lax cookie is withheld from exactly those, so the
// dashboard's state-changing forms cannot be forged from another origin.
//
// The token also decides which workspace the signed-in session sees, so a
// dashboard opened with a team's token cannot list another team's monitors.
//
// Sessions live in memory (internal/auth): a restart signs everyone out.
// There is no user database to invalidate against, and re-entering the token
// is the honest cost of a single shared credential.
const (
	loginPath  = "/login"
	logoutPath = "/logout"
)

// authExempt reports whether a dashboard path is served without a session.
// /login has to be reachable to sign in at all; the two OIDC paths for the same
// reason and in both directions — a visitor who is not signed in needs to start
// the flow, and the provider's callback arrives with nothing but a state and a
// code, so gating it would turn every callback into a redirect back to the
// start and never complete a login. /favicon.ico so an unauthenticated page
// load does not answer the icon request with a login redirect; /theme and
// /timezone because they set presentation cookies only — no data is read or
// written — and the sign-in page renders the same header (and therefore the
// same theme switch) as every other page.
func authExempt(path string) bool {
	switch path {
	case loginPath, logoutPath, "/favicon.ico", "/theme", "/timezone", oidcStartPath, oidcCallbackPath:
		return true
	default:
		return false
	}
}

// sessionCookie builds the cookie that carries a session id. Secure follows
// the request: the quickstart runs on plain HTTP on localhost, where a Secure
// cookie would simply never be stored (and a TLS-terminating proxy that
// forwards plain HTTP would break sign-in the same way — enable TLS at this
// process, or accept that the cookie is not marked Secure).
func sessionCookie(id string, secure bool, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:  auth.SessionCookie,
		Value: id,
		Path:  "/",
		// No page script needs the value, so XSS cannot read it.
		HttpOnly: true,
		// Lax, not Strict: Strict would also drop the cookie on an inbound
		// link to the dashboard, which reads as "signed out" for no gain —
		// Lax already withholds it from cross-site POSTs.
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(ttl.Seconds()),
	}
}

// WithAuth requires a live dashboard session once a token is configured, and
// enables the sign-in page. Nil — or an authenticator with no tokens, which
// is what an unset API_TOKEN produces — leaves the dashboard open, exactly as
// it was before authentication existed.
func (s *Server) WithAuth(a *auth.Authenticator) *Server {
	s.auth = a
	return s
}

func (s *Server) authEnabled() bool {
	return s.auth.Enabled()
}

// authMiddleware gates every dashboard route on a live session and scopes the
// request to the workspace that session was signed into. Exempt paths are the
// sign-in flow itself and the presentation-only cookie routes above.
//
// The scope comes from the session, which inherited it from the token at
// /login: the dashboard never asks the browser which workspace it wants.
func (s *Server) authMiddleware() func(http.Handler) http.Handler {
	if !s.authEnabled() {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ws, ok := s.auth.SessionWorkspace(auth.SessionID(r))
			if !ok {
				// Every dashboard client is a browser, so answer with the sign-in
				// page rather than a bodyless 401. 303 matters for the POSTs
				// (create, toggle, delete, retry): it converts them into a GET of
				// /login instead of a "resubmit form?" prompt, and deliberately
				// drops the body — an unauthenticated POST must not be replayed
				// once the operator signs in.
				http.Redirect(w, r, loginPath+"?next="+url.QueryEscape(safeNext(r.URL.RequestURI())), http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r.WithContext(workspace.With(r.Context(), ws)))
		})
	}
}

// safeNext keeps a post-sign-in redirect on this site. Only a site-absolute
// path is accepted: "//evil.example" (protocol-relative) and absolute URLs
// are dropped, so the sign-in link cannot be turned into an open redirect
// that makes the dashboard look like the source of an off-site page.
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.HasPrefix(v, `/\`) {
		return "/"
	}
	return v
}

// loginData is the sign-in page's scaffold, shared by the GET, the rejected
// token and a failed SSO round-trip, so the three cannot drift into offering
// different ways in. What the page offers depends on what the instance accepts:
// a token form only when a static token exists, an SSO button only when a
// provider does, and both when an operator is migrating from one to the other.
func (s *Server) loginData(r *http.Request, next, errMsg string) map[string]any {
	data := map[string]any{
		"Title":      "Sign in",
		"Next":       next,
		"LocalLogin": s.auth.HasStaticTokens(),
	}
	if p := s.auth.OIDC(); p.Enabled() {
		// The display name is the issuer's host, so the label on the button says
		// who the browser is about to be sent to.
		data["SSO"] = p.DisplayName()
	}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	return data
}

// loginPage renders the sign-in form. Already being signed in is not an
// error, it is just nothing to do here, so it redirects to the overview.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", s.loginData(r, safeNext(r.URL.Query().Get("next")), ""))
}

// login checks the submitted token and starts a session. The token never
// reaches the page, the log line or an error message: a failed sign-in is
// reported as "not accepted", with no detail about which token was tried.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	next := safeNext(r.PostFormValue("next"))

	id, ok := s.auth.Login(strings.TrimSpace(r.PostFormValue("token")))
	if !ok {
		s.log.Warn("dashboard sign-in rejected", "remote_addr", r.RemoteAddr)
		s.renderStatus(w, r, http.StatusUnauthorized, "login",
			s.loginData(r, next, "That token was not accepted."))
		return
	}

	http.SetCookie(w, sessionCookie(id, r.TLS != nil, s.auth.SessionTTL()))
	s.log.Info("dashboard signed in", "remote_addr", r.RemoteAddr)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// logout ends the session and clears the cookie. Signing out with no session
// (or with auth disabled) is harmless, and the redirect is the same, so a
// stale dashboard tab always lands somewhere it can act from.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.DropSession(auth.SessionID(r))

	c := sessionCookie("", r.TLS != nil, s.auth.SessionTTL())
	c.MaxAge = -1 // delete: also covers the browser-session (MaxAge 0) case
	http.SetCookie(w, c)
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}
