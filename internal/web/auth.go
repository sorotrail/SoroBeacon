package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
)

// Dashboard sign-in.
//
// The dashboard has no user accounts and is not going to grow any: it accepts
// the same static token as the API (API_TOKEN) and, on success, sets an
// HttpOnly session cookie. A cookie rather than HTTP Basic because Basic
// credentials are cached by the browser and replayed on requests the page did
// not initiate — including cross-site ones — while a SameSite=Lax cookie is
// withheld from exactly those, so the dashboard's state-changing forms cannot
// be forged from another origin.
//
// Sessions live in memory (internal/auth): a restart signs everyone out.
// There is no user database to invalidate against, and re-entering the token
// is the honest cost of a single shared credential.
const (
	loginPath  = "/login"
	logoutPath = "/logout"
)

// authExempt reports whether a dashboard path is served without a session.
// /login has to be reachable to sign in at all; /favicon.ico so an
// unauthenticated page load does not answer the icon request with a login
// redirect; /theme and /timezone because they set presentation cookies only —
// no data is read or written — and the sign-in page renders the same header
// (and therefore the same theme switch) as every other page.
func authExempt(path string) bool {
	switch path {
	case loginPath, logoutPath, "/favicon.ico", "/theme", "/timezone":
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
	s.a = a
	s.auth = a
	return s
}

func (s *Server) authEnabled() bool {
	return s.auth.Enabled()
}

// authMiddleware gates every dashboard route on a live session. Exempt paths
// are the sign-in flow itself and the presentation-only cookie routes above.
func (s *Server) authMiddleware() func(http.Handler) http.Handler {
	if !s.authEnabled() {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authExempt(r.URL.Path) || s.auth.HasSession(auth.SessionID(r)) {
				next.ServeHTTP(w, r)
				return
			}
			// Every dashboard client is a browser, so answer with the sign-in
			// page rather than a bodyless 401. 303 matters for the POSTs
			// (create, toggle, delete, retry): it converts them into a GET of
			// /login instead of a "resubmit form?" prompt, and deliberately
			// drops the body — an unauthenticated POST must not be replayed
			// once the operator signs in.
			http.Redirect(w, r, loginPath+"?next="+url.QueryEscape(safeNext(r.URL.RequestURI())), http.StatusSeeOther)
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

// loginPage renders the sign-in form. Already being signed in is not an
// error, it is just nothing to do here, so it redirects to the overview.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", map[string]any{
		"Title": "Sign in",
		"Next":  safeNext(r.URL.Query().Get("next")),
	})
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
		s.renderStatus(w, r, http.StatusUnauthorized, "login", map[string]any{
			"Title": "Sign in",
			"Next":  next,
			"Error": "That token was not accepted.",
		})
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
