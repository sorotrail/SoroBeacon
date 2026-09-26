package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// API tokens are scoped, expiring, revocable credentials: the thing to hand a
// CI job so it can create one monitor without being able to delete every
// channel, and the thing to retire when a pipeline is decommissioned.
//
// The raw secret appears in exactly one response — the 201 that mints it. It is
// never stored (only its digest is, see auth.HashToken), never listed, and
// never logged, so "shown once" is a property of this code rather than a
// warning on a screen. The dashboard page says the same thing in prose.

// apiToken is one token as the API describes it: enough to recognise and audit,
// with nothing to steal.
type apiToken struct {
	ID         int64        `json:"id"`
	Name       string       `json:"name"`
	Prefix     string       `json:"prefix"`
	Scopes     []auth.Scope `json:"scopes"`
	CreatedAt  time.Time    `json:"created_at"`
	ExpiresAt  *time.Time   `json:"expires_at"`
	LastUsedAt *time.Time   `json:"last_used_at"`
	RevokedAt  *time.Time   `json:"revoked_at"`
}

// createdToken is the one response that carries a secret, which is why it is a
// separate type embedding the displayable one rather than a field on it.
type createdToken struct {
	apiToken
	Token string `json:"token"`
}

// createTokenRequest is POST /tokens' body. ExpiresIn is a Go duration
// ("720h"), deliberately not a bare number: a token whose owner meant 720
// *days* is the failure this field exists to prevent.
type createTokenRequest struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresIn string   `json:"expires_in"`
}

func apiTokenOf(t *auth.Token) apiToken {
	out := apiToken{
		ID: t.ID, Name: t.Name, Prefix: t.Prefix,
		Scopes: t.Scopes, CreatedAt: t.CreatedAt.UTC(),
	}
	if out.Scopes == nil {
		// A token with no scopes is a real state (it authenticates and can
		// call nothing), and JSON null would read as "field missing" to a
		// client that is deciding whether to trust the row.
		out.Scopes = []auth.Scope{}
	}
	for _, src := range []struct {
		dst **time.Time
		val time.Time
	}{{&out.ExpiresAt, t.ExpiresAt}, {&out.LastUsedAt, t.LastUsedAt}, {&out.RevokedAt, t.RevokedAt}} {
		if !src.val.IsZero() {
			v := src.val.UTC()
			*src.dst = &v
		}
	}
	return out
}

// createToken mints a token and returns its secret once.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeErr(w, r, http.StatusServiceUnavailable, "token management is not configured")
		return
	}
	var in createTokenRequest
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if len(name) > auth.MaxTokenNameLen {
		writeValidation(w, r, []FieldError{{
			Field:  "name",
			Reason: "name must be at most " + strconv.Itoa(auth.MaxTokenNameLen) + " characters",
		}})
		return
	}
	scopes, err := auth.ParseScopes(in.Scopes)
	if err != nil {
		writeValidation(w, r, []FieldError{{Field: "scopes", Reason: err.Error()}})
		return
	}
	// A credential that can call nothing is a mistake in the request, not a
	// useful state: it authenticates, then fails on the first route.
	if len(scopes) == 0 {
		writeValidation(w, r, []FieldError{{
			Field:  "scopes",
			Reason: "a token needs at least one scope (accepted: " + auth.JoinScopes(auth.AllScopes) + ")",
		}})
		return
	}
	// The escalation guard: a scoped token must not mint a stronger one, or
	// tokens:write would be a road back to full control of the instance.
	if !callerMayGrant(r, scopes) {
		writeErr(w, r, http.StatusForbidden, "a scoped token cannot mint a token with scopes it does not hold")
		return
	}
	var expiresAt time.Time
	if v := strings.TrimSpace(in.ExpiresIn); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeValidation(w, r, []FieldError{{
				Field:  "expires_in",
				Reason: "expires_in is a Go duration (for example 168h for seven days); omit it for a token that never expires",
			}})
			return
		}
		if d <= 0 {
			writeValidation(w, r, []FieldError{{
				Field:  "expires_in",
				Reason: "expires_in must be positive; omit it for a token that never expires",
			}})
			return
		}
		expiresAt = time.Now().Add(d)
	}

	raw, tok, err := s.tokens.Mint(r.Context(), name, scopes, expiresAt)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Cache-Control is set by writeJSON: a proxy that cached this response
	// would hand the secret to the next caller.
	writeJSON(w, http.StatusCreated, createdToken{apiToken: apiTokenOf(tok), Token: raw})
}

// listTokens shows the caller's tokens. Prefix and timestamps, never a secret
// and never the digest either: a hash is not recoverable, but it is what an
// offline attacker would get to work on.
func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeErr(w, r, http.StatusServiceUnavailable, "token management is not configured")
		return
	}
	toks, err := s.tokens.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]apiToken, 0, len(toks))
	for i := range toks {
		out = append(out, apiTokenOf(&toks[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// revokeToken retires one. The row survives so the audit trail (name, prefix,
// scopes, when it was last used) outlives the credential.
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeErr(w, r, http.StatusServiceUnavailable, "token management is not configured")
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusNotFound, "not found")
		return
	}
	if err := s.tokens.Revoke(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The same answer for "no such token" and "someone else's token".
			writeErr(w, r, http.StatusNotFound, "not found")
			return
		}
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// callerMayGrant reports whether the request's credential may mint a token
// carrying want.
//
// A request that never passed the auth middleware — a deployment with no
// API_TOKEN, where the middleware steps aside — has no principal, and is
// treated as unrestricted. That is not a loophole: in such a deployment every
// caller already holds the whole instance, so refusing to mint here would hide
// a working endpoint behind a message about scopes.
func callerMayGrant(r *http.Request, want []auth.Scope) bool {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		return true
	}
	return p.CanGrant(want)
}
