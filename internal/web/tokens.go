package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// The dashboard's API-token page: mint, list, revoke.
//
// The listing shows a prefix, the scope list and three timestamps. It cannot
// show a token even by accident: only a token's digest is stored
// (auth.HashToken), so the page has nothing to reveal. What it does show, once,
// is the secret a mint just produced — rendered inline in the mint response
// rather than carried through a redirect, because a redirect would put it in a
// URL, and URLs are kept by browsers, proxies and `Referer` headers.

// tokenRow is one api_tokens row as the page displays it. The state is worked
// out here rather than in the template so "revoked", "expired" and "active"
// mean one thing in one place.
type tokenRow struct {
	ID           int64
	Name         string
	Prefix       string
	Scopes       string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	LastUsedAt   time.Time
	State        string
	Revoked      bool
	NeverExpires bool
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.listTokens(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "tokens", s.tokenPageData(r, list, ""))
}

// createToken mints a token and renders its secret on this response only.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	if s.tokenMgr == nil {
		s.renderStatus(w, r, http.StatusServiceUnavailable, "error", map[string]any{
			"Title":   "Unavailable",
			"Message": "Token management is not configured on this instance.",
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	scopes, err := auth.ParseScopes(r.PostForm["scopes"])
	if err == nil && len(scopes) == 0 {
		err = fmt.Errorf("pick at least one scope: a token with none can call nothing")
	}
	var expiresAt time.Time
	if err == nil {
		if expiresAt, err = parseExpiry(r.FormValue("expires_in")); err != nil {
			err = fmt.Errorf("%w", err)
		}
	}
	if err != nil {
		list, lerr := s.listTokens(r)
		if lerr != nil {
			s.fail(w, lerr)
			return
		}
		data := s.tokenPageData(r, list, "")
		data["FormError"] = err.Error()
		s.renderStatus(w, r, http.StatusBadRequest, "tokens", data)
		return
	}

	raw, tok, err := s.tokenMgr.Mint(r.Context(), name, scopes, expiresAt)
	if err != nil {
		s.fail(w, err)
		return
	}
	list, err := s.listTokens(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The name and scopes are worth a log line; the credential is not, and the
	// digest is not either — a log is the one place an attacker can read
	// without authenticating.
	s.log.Info("api token created", "token_id", tok.ID, "name", tok.Name,
		"scopes", auth.JoinScopes(tok.Scopes))

	data := s.tokenPageData(r, list, raw)
	data["NewName"] = tok.Name
	s.render(w, r, "tokens", data)
}

// revokeToken retires one token. The row stays, so the listing still answers
// "what was this, and when did it last get used".
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if s.tokenMgr == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.tokenMgr.Revoke(r.Context(), id); err != nil {
		// Another workspace's token and a token that does not exist are the
		// same 404 here, as they are through the API.
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	s.log.Info("api token revoked", "token_id", id)
	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

// tokenPageData is the page's shared scaffold, so the GET, the rejected form
// and the successful mint cannot each drift into rendering a different set of
// things around the same table.
func (s *Server) tokenPageData(r *http.Request, list []auth.Token, newToken string) map[string]any {
	now := time.Now()
	rows := make([]tokenRow, 0, len(list))
	for _, t := range list {
		rows = append(rows, tokenRow{
			ID:           t.ID,
			Name:         t.Name,
			Prefix:       t.Prefix,
			Scopes:       auth.JoinScopes(t.Scopes),
			CreatedAt:    t.CreatedAt,
			ExpiresAt:    t.ExpiresAt,
			LastUsedAt:   t.LastUsedAt,
			State:        tokenState(t, now),
			Revoked:      !t.RevokedAt.IsZero(),
			NeverExpires: t.ExpiresAt.IsZero(),
		})
	}
	return map[string]any{
		"Title":    "API Tokens",
		"Active":   "tokens",
		"Tokens":   rows,
		"Scopes":   auth.AllScopes,
		"NewToken": newToken,
		"Disabled": s.tokenMgr == nil,
	}
}

// WithTokens enables the dashboard's API-token page. It is the same
// *auth.Manager the JSON API is given, so a token minted from either side is
// subject to the same rules about what it may hold.
func (s *Server) WithTokens(m *auth.Manager) *Server {
	s.tokenMgr = m
	return s
}

func (s *Server) listTokens(r *http.Request) ([]auth.Token, error) {
	if s.tokenMgr == nil {
		return nil, nil
	}
	return s.tokenMgr.List(r.Context())
}

// parseExpiry reads the form's expiry choice. The dashboard offers whole days
// because that is the unit an operator reasons in; an empty value or "never" is
// a token with no expiry, and the API's free-form duration is the API's affair.
func parseExpiry(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "never" {
		return time.Time{}, nil
	}
	days, err := strconv.Atoi(v)
	if err != nil || days < 1 || days > 3650 {
		return time.Time{}, fmt.Errorf("expiry must be a whole number of days between 1 and 3650, or never")
	}
	return time.Now().AddDate(0, 0, days), nil
}

// tokenState is the one-word status of a token. Revoked and expired are
// distinguishable *here*, on the operator's own listing, precisely because
// they are indistinguishable to a caller presenting a token: the auth path
// answers both with ErrInvalidToken.
func tokenState(t auth.Token, now time.Time) string {
	switch {
	case !t.RevokedAt.IsZero():
		return "revoked"
	case t.ExpiresAt.IsZero():
		return "no expiry"
	case !now.Before(t.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}
