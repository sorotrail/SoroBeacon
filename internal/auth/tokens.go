package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// A token created here is a database-backed credential: scopes, an expiry, a
// revocation flag and a last-used timestamp, so an operator can hand CI a
// credential that can create a monitor and nothing else, and retire it without
// rotating the instance-wide API_TOKEN.
//
// Two kinds of credential coexist on purpose:
//
//   - The static tokens from API_TOKEN / WORKSPACE_TOKENS carry every scope.
//     They are instance configuration, they authenticate the dashboard login,
//     and there is exactly one per operator, so "the owner" is the reasonable
//     grant. They cannot be revoked per-token, which is the gap this file
//     closes.
//   - A Token below is one row: revocable, expiring, and only as powerful as
//     the scopes it was minted with.
//
// The scopes themselves are checked by the HTTP layer (internal/api), which is
// where a route's resource and verb are known. This file owns what a scope *is*
// and what a token holds; the route table owns which route needs which scope.

const (
	// TokenPrefix marks a database-backed token. The prefix is what lets an
	// authenticating request try the (indexed) token lookup without a query for
	// every static bearer, and it makes a leaked string self-identifying in a
	// scanner's findings.
	TokenPrefix = "sb_"

	// tokenSecretBytes is the entropy per token: 256 bits, base64url-encoded,
	// the same guess-resistance budget as a session id.
	tokenSecretBytes = 32

	// tokenPrefixLen is how many characters of the secret the dashboard shows.
	// Long enough to tell two tokens apart at a glance, far too short to be
	// worth guessing against, and it is a *prefix of the secret*, never a
	// fragment of its hash.
	tokenPrefixLen = 8

	// MaxTokenNameLen bounds the human label. It is stored, listed and logged
	// (never the secret), so it is the field a caller can use to shout.
	MaxTokenNameLen = 64

	// defaultTokenName is the label the dashboard and API fall back to when a
	// request supplies none, so a listing always has something to show.
	defaultTokenName = "unnamed"
)

// ErrInvalidToken reports that a bearer string is not a usable token: unknown,
// revoked and expired all produce it. Distinguishing them would tell a caller
// who guessed a token's hash nothing about whether they were close, while
// telling an attacker which of their own tokens expired saves them nothing.
var ErrInvalidToken = errors.New("invalid token")

// Scope is one resource:action permission, e.g. ScopeMonitorsRead. Resource
// level (not per route) is the granularity chosen here: a route table maps many
// routes onto one scope, so granting "monitors:read" covers GET /monitors,
// GET /monitors/{id} and GET /monitors/{id}/rules without a per-endpoint
// enumeration that has to be revisited every time an endpoint is added.
type Scope string

const (
	ScopeMonitorsRead   Scope = "monitors:read"
	ScopeMonitorsWrite  Scope = "monitors:write"
	ScopeChannelsRead   Scope = "channels:read"
	ScopeChannelsWrite  Scope = "channels:write"
	ScopeAlertsRead     Scope = "alerts:read"
	ScopeAlertsWrite    Scope = "alerts:write"
	ScopeTemplatesRead  Scope = "templates:read"
	ScopeTemplatesWrite Scope = "templates:write"
	ScopeStatsRead      Scope = "stats:read"
	ScopeTokensRead     Scope = "tokens:read"
	ScopeTokensWrite    Scope = "tokens:write"

	// ScopeAll is a token that carries every scope. Minting one is the same
	// statement as configuring API_TOKEN, so the dashboard labels it.
	ScopeAll Scope = "*"

	// ScopeNone marks a route that needs a valid credential and no particular
	// permission — GET /version, which a deployment cannot avoid exposing
	// anyway. It is a distinct value rather than "no entry in the table": the
	// table's absence of an entry means *deny*, and a route that means to ask
	// for nothing has to say so.
	ScopeNone Scope = "none"
)

// AllScopes is the vocabulary a mint request is validated against, and what the
// dashboard offers. It is a slice so the UI lists scopes in a stable order;
// validScopes is the lookup the validator uses.
//
// The list covers resources the *API* serves, because a scope is only ever
// checked by the API's route table. Saved searches are a tenant resource too,
// but they exist only on the dashboard, and a scoped token cannot sign in there
// (see Login) — so a "searches:read" scope would be a permission that grants
// nothing, which is worse than no scope at all.
var AllScopes = []Scope{
	ScopeMonitorsRead, ScopeMonitorsWrite,
	ScopeChannelsRead, ScopeChannelsWrite,
	ScopeAlertsRead, ScopeAlertsWrite,
	ScopeTemplatesRead, ScopeTemplatesWrite,
	ScopeStatsRead,
	ScopeTokensRead, ScopeTokensWrite,
	ScopeAll,
}

var validScopes = func() map[Scope]bool {
	m := make(map[Scope]bool, len(AllScopes)+1)
	for _, s := range AllScopes {
		m[s] = true
	}
	m[ScopeNone] = true
	return m
}()

// ParseScopes validates and normalises a requested scope list: trimmed,
// deduplicated, sorted, and rejected as a whole if any name is outside the
// vocabulary. The error names the rejected values (a scope is not a secret) and
// lists what is accepted, so a caller can fix the request without reading source.
func ParseScopes(raw []string) ([]Scope, error) {
	seen := make(map[Scope]bool, len(raw))
	out := make([]Scope, 0, len(raw))
	var bad []string
	for _, r := range raw {
		s := Scope(strings.TrimSpace(r))
		if s == "" {
			continue
		}
		if !validScopes[s] {
			bad = append(bad, string(s))
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("unknown scope(s): %s (accepted: %s)",
			strings.Join(bad, ", "), JoinScopes(AllScopes))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// JoinScopes renders a scope list for storage and display. The stored form is a
// comma-separated list rather than one row per scope: scopes are read together
// with the token on every request, and a join table would add a query to the
// hot path for a value that never gets queried on its own.
func JoinScopes(scopes []Scope) string {
	parts := make([]string, len(scopes))
	for i, s := range scopes {
		parts[i] = string(s)
	}
	return strings.Join(parts, ",")
}

// SplitScopes is JoinScopes' inverse. Unknown stored values survive: a token
// minted by a newer binary must still list correctly on an older one rather
// than silently losing privileges, which is the wrong direction to fail.
func SplitScopes(s string) []Scope {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]Scope, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, Scope(p))
		}
	}
	return out
}

// Token is one API token row as this package needs it. Hash and not secret:
// the plaintext exists only inside Mint's return value, so no code path here can
// hand it to a log line or a response body by accident.
type Token struct {
	ID     int64
	Name   string
	Prefix string
	// Hash is the hex SHA-256 of the secret. Never serialised.
	Hash string `json:"-"`
	// Workspace is the tenant the token belongs to, read from its row. The
	// request that authenticates has no workspace yet — resolving it is this
	// value's job — so the tenancy cannot come from the caller.
	Workspace  workspace.ID `json:"-"`
	Scopes     []Scope
	ExpiresAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time
	CreatedAt  time.Time
}

// Live reports whether the token may authenticate right now. Revoked and
// expired are separate columns but one answer: the caller that gets ErrInvalidToken
// cannot tell which, and neither can anyone guessing.
func (t *Token) Live(now time.Time) bool {
	if t == nil {
		return false
	}
	if !t.RevokedAt.IsZero() {
		return false
	}
	if !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt) {
		return false
	}
	return true
}

// Principal is the caller a request turned out to come from: which tenant it
// acts on, and what it is allowed to do there.
type Principal struct {
	Workspace workspace.ID
	// restricted is true for a database token, for which scopes is the whole
	// grant. It is a flag rather than "scopes is nil" because the list is
	// stored as a comma-separated string: a token minted with no scopes
	// round-trips through the database as no scopes at all, and an absent list
	// must not read back as the unrestricted credential it never was.
	restricted bool
	// scopes is the grant, empty for a credential that carries none.
	scopes  []Scope
	tokenID int64
	// Name is the token's label, for log lines that identify *which* credential
	// was used without containing any part of it.
	Name string
}

// Allowed reports whether this caller may perform the action a route asks for.
// ScopeNone is allowed for everyone: it is the entry's statement that only a
// valid credential is needed.
//
// Within one resource, write carries read. The route table asks a creating
// route for `monitors:write` and a listing route for `monitors:read`, so
// without the implication a token that adds a monitor cannot read back the id
// it just got — an operator granting "let CI manage monitors" would get a
// credential that half-works, and the fix would be to grant both scopes every
// time. The reverse never holds, and a grant stays readable: the scope list on
// the listing page is the permission, not a hint about a wider one.
func (p *Principal) Allowed(s Scope) bool {
	if p == nil {
		return false
	}
	if s == ScopeNone || !p.restricted {
		return true
	}
	for _, have := range p.scopes {
		if have == s || have == ScopeAll {
			return true
		}
		if strings.HasSuffix(string(s), ":read") && have == Scope(strings.TrimSuffix(string(s), ":read")+":write") {
			return true
		}
	}
	return false
}

// TokenID identifies the database token behind this caller, 0 for a static
// token or a session.
func (p *Principal) TokenID() int64 {
	if p == nil {
		return 0
	}
	return p.tokenID
}

// Unrestricted reports a credential the route table does not constrain: a static
// token from API_TOKEN / WORKSPACE_TOKENS, or a dashboard session minted from
// one. The API uses it to decide whether to consult the table at all, and the
// dashboard uses it to tell an operator that a token they are about to mint is
// the all-but-unrevocable kind.
func (p *Principal) Unrestricted() bool {
	return p != nil && !p.restricted
}

// TokenStore is the persistence a Manager needs. It is store's own shape — the
// methods are store.Store's, and store.Store satisfies it — declared here so
// this package stays testable without a database and so the token vocabulary has
// one owner. Every method takes a context whose workspace scopes the rows it
// sees, except TokenByHash, which runs before tenancy is known; see
// internal/store/workspace_scope.go.
type TokenStore interface {
	CreateAPIToken(ctx context.Context, t *Token) error
	// TokenByHash resolves a credential to its row before tenancy is known, so
	// it is the one method that ignores ctx's workspace. found is false for a
	// digest no row owns, which keeps "no such token" distinct from "the
	// database failed" without this package importing the store's errors.
	TokenByHash(ctx context.Context, hash string) (token *Token, found bool, err error)
	ListAPITokens(ctx context.Context) ([]Token, error)
	RevokeAPIToken(ctx context.Context, id int64) error
	TouchAPIToken(ctx context.Context, id int64, at time.Time) error
}

// Manager mints, authenticates and retires database tokens.
type Manager struct {
	store TokenStore
	log   *slog.Logger
	now   func() time.Time
}

// NewManager builds a Manager over token storage. A nil logger discards the
// bookkeeping warnings; storage failures that do not affect the answer are
// reported there rather than turned into a 401, because a token whose last-used
// stamp could not be written is still a valid token.
func NewManager(st TokenStore, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{store: st, log: log, now: time.Now}
}

func (m *Manager) withNow(f func() time.Time) *Manager {
	if f != nil {
		m.now = f
	}
	return m
}

// Mint creates a token with the given name and scopes and returns its raw
// secret exactly once. expiresAt is the zero time for a token that never
// expires; the caller decides that, and the dashboard says so out loud.
//
// The context's workspace owns the row, so a token minted inside a request can
// only ever authenticate into the tenant that minted it — there is no argument
// through which a caller could ask for someone else's workspace.
func (m *Manager) Mint(ctx context.Context, name string, scopes []Scope, expiresAt time.Time) (string, *Token, error) {
	secret, err := newTokenSecret()
	if err != nil {
		// No entropy means no unpredictable credential, which is a refusal
		// rather than a fallback to something guessable.
		return "", nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultTokenName
	}
	if len(name) > MaxTokenNameLen {
		return "", nil, fmt.Errorf("name must be at most %d characters", MaxTokenNameLen)
	}
	t := &Token{
		Name:   name,
		Prefix: secret[:len(TokenPrefix)+tokenPrefixLen],
		Hash:   HashToken(secret),
		// A nil scope list means "unrestricted", which a minted token must
		// never be, so an empty request stores an empty (not nil) list.
		Scopes:    append([]Scope{}, scopes...),
		ExpiresAt: expiresAt.UTC().Truncate(time.Second),
	}
	if err := m.store.CreateAPIToken(ctx, t); err != nil {
		return "", nil, err
	}
	return secret, t, nil
}

// Authenticate resolves a raw bearer token to its caller. Unknown, revoked and
// expired tokens all yield ErrInvalidToken, and nothing about the error or its
// text says which.
func (m *Manager) Authenticate(ctx context.Context, raw string) (*Principal, error) {
	if m == nil || m.store == nil {
		return nil, ErrInvalidToken
	}
	t, found, err := m.store.TokenByHash(ctx, HashToken(raw))
	if err != nil {
		m.log.Warn("api token lookup failed", "err", err)
		return nil, ErrInvalidToken
	}
	if !found {
		return nil, ErrInvalidToken
	}
	now := m.now()
	if !t.Live(now) {
		return nil, ErrInvalidToken
	}
	if !t.LastUsedAt.IsZero() && now.Sub(t.LastUsedAt) < touchInterval {
		// A busy CI job would otherwise write one row per request. The stamp is
		// for finding stale tokens, and "used within the last minute" answers
		// that question just as well as "used 40 milliseconds ago".
		return principalFor(t), nil
	}
	if err := m.store.TouchAPIToken(workspace.With(ctx, t.Workspace), t.ID, now.UTC().Truncate(time.Second)); err != nil {
		m.log.Warn("api token last_used update failed", "token_id", t.ID, "err", err)
	}
	return principalFor(t), nil
}

// touchInterval bounds how often a token's last_used_at is rewritten.
const touchInterval = time.Minute

// principalFor is the one place a database token becomes a caller, so
// restricted: true cannot be forgotten the way an implied "non-nil scopes"
// check could be.
func principalFor(t *Token) *Principal {
	return &Principal{
		Workspace:  t.Workspace,
		restricted: true,
		scopes:     t.Scopes,
		tokenID:    t.ID,
		Name:       t.Name,
	}
}

// List returns the caller workspace's tokens, newest first, without secrets or
// hashes.
func (m *Manager) List(ctx context.Context) ([]Token, error) {
	if m == nil || m.store == nil {
		return nil, nil
	}
	return m.store.ListAPITokens(ctx)
}

// Revoke retires one of the caller workspace's tokens. A token that is already
// revoked, or belongs to another workspace, reports ErrNotFound: revocation is
// idempotent from the caller's point of view, and it must not confirm that
// another tenant holds an id this one could guess.
func (m *Manager) Revoke(ctx context.Context, id int64) error {
	if m == nil || m.store == nil {
		return ErrInvalidToken
	}
	return m.store.RevokeAPIToken(ctx, id)
}

// HashToken is the credential digest: hex SHA-256 over the raw secret.
//
// SHA-256 rather than bcrypt/scrypt/argon2 because these tokens are 256 bits of
// crypto/rand, not a human password. An offline attack against full entropy is
// infeasible regardless of the hash, so a slow KDF buys nothing here while
// costing two things this path needs: an equality lookup (the digest is
// indexed, so authenticating is one indexed read instead of a slow compare per
// stored token), and the same construction the static-token path already uses
// in this package.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// IsTokenBearer reports whether a bearer string is shaped like a database
// token, which is what lets the authenticator skip the token table for the
// static tokens it also serves.
func IsTokenBearer(raw string) bool {
	return strings.HasPrefix(raw, TokenPrefix)
}

func newTokenSecret() (string, error) {
	buf := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("auth: generate token: " + err.Error())
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// principalContextKey is the request-context key a resolved Principal is carried
// under. Handlers reach it to ask "who is calling, and may they grant what is
// being asked for", which is the one question a scope list cannot answer from the
// workspace alone.
type principalContextKey struct{}

// WithPrincipal returns ctx carrying p. The auth middleware calls it; nothing
// else should, because a principal assembled anywhere other than Principal is a
// caller-chosen permission set.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFromContext is the other half of WithPrincipal. ok is false when the
// request never passed through the auth middleware — a nil result is treated as
// "no permission" by every caller, never as "unrestricted".
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(*Principal)
	return p, ok && p != nil
}

// CanGrant reports whether this caller may mint a token carrying want. A scoped
// token cannot hand out a wider grant than it holds, because tokens:write would
// otherwise be a route to full control of the instance — the escalation is the
// reason service tokens are scoped in the first place. An unrestricted caller
// (a static token, or a session from one) can grant anything, which is the
// operator's own reach.
func (p *Principal) CanGrant(want []Scope) bool {
	if p == nil {
		return false
	}
	if !p.restricted {
		return true
	}
	for _, w := range want {
		if w == ScopeAll {
			// "*" is the whole vocabulary including scopes added later, so it
			// is not something a scoped token can hold or pass on.
			return false
		}
		if !p.Allowed(w) {
			return false
		}
	}
	return true
}
