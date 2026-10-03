package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// fakeTokens is the storage a Manager needs, in memory. It records what it was
// asked to store, so a test can assert on the *stored* row — the only place a
// secret could be leaked besides a log line.
type fakeTokens struct {
	byHash    map[string]*Token
	touches   []int64
	touchErr  error
	next      int64
	revokeErr error
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{byHash: map[string]*Token{}}
}

// wsOf mirrors the store's own resolution: a context that names no workspace
// belongs to the default one.
func wsOf(ctx context.Context) workspace.ID {
	if ws, ok := workspace.From(ctx); ok {
		return ws
	}
	return workspace.Default
}

func (f *fakeTokens) CreateAPIToken(ctx context.Context, t *Token) error {
	f.next++
	t.ID = f.next
	t.Workspace = wsOf(ctx)
	stored := *t
	f.byHash[stored.Hash] = &stored
	return nil
}

func (f *fakeTokens) TokenByHash(_ context.Context, hash string) (*Token, bool, error) {
	t, ok := f.byHash[hash]
	if !ok {
		return nil, false, nil
	}
	copied := *t
	return &copied, true, nil
}

func (f *fakeTokens) ListAPITokens(ctx context.Context) ([]Token, error) {
	ws := wsOf(ctx)
	var out []Token
	for _, t := range f.byHash {
		if t.Workspace == ws {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (f *fakeTokens) RevokeAPIToken(ctx context.Context, id int64) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	ws := wsOf(ctx)
	for _, t := range f.byHash {
		if t.ID != id || t.Workspace != ws {
			continue
		}
		if t.RevokedAt.IsZero() {
			t.RevokedAt = time.Now().UTC().Truncate(time.Second)
		}
		return nil
	}
	return errors.New("not found")
}

func (f *fakeTokens) TouchAPIToken(_ context.Context, id int64, at time.Time) error {
	if f.touchErr != nil {
		return f.touchErr
	}
	f.touches = append(f.touches, id)
	for _, t := range f.byHash {
		if t.ID == id {
			t.LastUsedAt = at
			return nil
		}
	}
	return nil
}

var testNow = time.Unix(1_800_000_000, 0).UTC()

// minted builds a Manager over fresh storage with a fixed clock and mints one
// token in the "acme" workspace, returning the secret, the row, and both pieces.
func minted(t *testing.T, scopes ...Scope) (string, *Token, *Manager, *fakeTokens) {
	t.Helper()
	st := newFakeTokens()
	m := NewManager(st, nil).withNow(func() time.Time { return testNow })
	raw, tok, err := m.Mint(workspace.With(context.Background(), "acme"), "ci", scopes, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return raw, tok, m, st
}

func TestParseScopesNormalisesAndRejects(t *testing.T) {
	got, err := ParseScopes([]string{" monitors:write ", "monitors:read", "monitors:read", ""})
	if err != nil {
		t.Fatalf("ParseScopes: %v", err)
	}
	// Trimmed, deduplicated and sorted, so two requests that mean the same thing
	// store the same bytes.
	want := []Scope{ScopeMonitorsRead, ScopeMonitorsWrite}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ParseScopes = %v, want %v", got, want)
	}

	_, err = ParseScopes([]string{"monitors:read", "monitor:write", "channels:*"})
	if err == nil {
		t.Fatal("unknown scopes must be an error, not a dropped entry")
	}
	// The error names the offenders and the vocabulary: a scope is not a secret,
	// and a caller should fix the request without reading source.
	for _, want := range []string{"monitor:write", "channels:*", "monitors:read"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestScopeJoinSplitRoundTrip(t *testing.T) {
	scopes := []Scope{ScopeStatsRead, ScopeAll}
	stored := JoinScopes(scopes)
	if stored != "stats:read,*" {
		t.Fatalf("JoinScopes = %q", stored)
	}
	back := SplitScopes(stored)
	if len(back) != 2 || back[0] != ScopeStatsRead || back[1] != ScopeAll {
		t.Fatalf("SplitScopes(%q) = %v", stored, back)
	}
	// Empty storage means no scopes, and must not become a slice holding "".
	if got := SplitScopes("  "); got != nil {
		t.Fatalf("SplitScopes(%q) = %v, want nil", "  ", got)
	}
	// A scope from a newer binary survives rather than vanishing: losing
	// privileges silently is the wrong direction to fail.
	if got := SplitScopes("monitors:read,future:thing"); len(got) != 2 || got[1] != "future:thing" {
		t.Fatalf("SplitScopes kept unknown scope = %v", got)
	}
}

func TestTokenLive(t *testing.T) {
	now := testNow
	for _, tc := range []struct {
		name  string
		token Token
		live  bool
	}{
		{"plain", Token{}, true},
		{"revoked", Token{RevokedAt: now.Add(-time.Hour)}, false},
		{"not yet expired", Token{ExpiresAt: now.Add(time.Minute)}, true},
		// The boundary is inclusive: a token whose expires_at has arrived is
		// finished, not "still good for this second".
		{"expiring now", Token{ExpiresAt: now}, false},
		{"expired", Token{ExpiresAt: now.Add(-time.Second)}, false},
		{"revoked and expired", Token{RevokedAt: now, ExpiresAt: now.Add(-time.Hour)}, false},
	} {
		if got := tc.token.Live(now); got != tc.live {
			t.Errorf("%s: Live = %v, want %v", tc.name, got, tc.live)
		}
	}
	var nilToken *Token
	if nilToken.Live(now) {
		t.Error("nil token must not be live")
	}
}

func TestMintShape(t *testing.T) {
	raw, tok, _, st := minted(t, ScopeMonitorsRead)

	if !strings.HasPrefix(raw, TokenPrefix) {
		t.Fatalf("secret %q should start with %q", raw, TokenPrefix)
	}
	// 32 random bytes, base64url without padding.
	if len(raw) != len(TokenPrefix)+43 {
		t.Fatalf("secret length = %d, want %d", len(raw), len(TokenPrefix)+43)
	}
	// The display prefix is the leading characters of the secret: enough to tell
	// two tokens apart, far too little to be worth guessing against.
	if want := raw[:len(TokenPrefix)+tokenPrefixLen]; tok.Prefix != want {
		t.Errorf("prefix = %q, want %q", tok.Prefix, want)
	}
	if tok.Hash == "" || tok.Hash != HashToken(raw) {
		t.Errorf("stored hash = %q, want the digest of the secret", tok.Hash)
	}
	if strings.Contains(tok.Hash, strings.TrimPrefix(raw, TokenPrefix)) {
		t.Errorf("stored row leaks the secret: %q", tok.Hash)
	}
	if tok.Workspace != "acme" {
		t.Errorf("workspace = %q, want the context's tenant", tok.Workspace)
	}

	// Two mints never repeat a secret.
	other, _, _, _ := minted(t, ScopeMonitorsRead)
	if other == raw {
		t.Error("tokens must not repeat a secret")
	}

	// Nothing in a stored row contains the plaintext.
	for _, stored := range st.byHash {
		if strings.Contains(stored.Name+"|"+stored.Prefix+"|"+stored.Hash, strings.TrimPrefix(raw, TokenPrefix)) {
			t.Errorf("stored row %+v leaks the secret", stored)
		}
	}
}

func TestMintDefaultsAndLimits(t *testing.T) {
	st := newFakeTokens()
	m := NewManager(st, nil)

	_, tok, err := m.Mint(context.Background(), "   ", []Scope{ScopeStatsRead}, time.Time{})
	if err != nil {
		t.Fatalf("Mint with no name: %v", err)
	}
	if tok.Name != defaultTokenName {
		t.Errorf("name = %q, want the fallback %q", tok.Name, defaultTokenName)
	}
	if tok.Workspace != workspace.Default {
		t.Errorf("workspace = %q, want default for a context with no tenant", tok.Workspace)
	}

	// An empty scope list stays empty rather than becoming nil, because nil is
	// what "unrestricted" reads back as through the storage round trip.
	_, tok, err = m.Mint(context.Background(), "none", nil, time.Time{})
	if err != nil {
		t.Fatalf("Mint with no scopes: %v", err)
	}
	if tok.Scopes == nil {
		t.Error("a minted token with no scopes must hold an empty list, not nil")
	}

	if _, _, err := m.Mint(context.Background(), strings.Repeat("x", MaxTokenNameLen+1), []Scope{ScopeAll}, time.Time{}); err == nil {
		t.Error("an over-long name must be refused")
	}
}

func TestAuthenticateLiveToken(t *testing.T) {
	st := newFakeTokens()
	m := NewManager(st, nil).withNow(func() time.Time { return testNow })
	ctx := workspace.With(context.Background(), "acme")

	raw, tok, err := m.Mint(ctx, "ci", []Scope{ScopeMonitorsRead}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	p, err := m.Authenticate(ctx, raw)
	if err != nil {
		t.Fatalf("Authenticate a live token: %v", err)
	}
	if p.Workspace != "acme" {
		t.Errorf("workspace = %q, want the tenant from the token's row", p.Workspace)
	}
	if p.Unrestricted() {
		t.Error("a database token is always scope-restricted")
	}
	if !p.Allowed(ScopeMonitorsRead) || p.Allowed(ScopeChannelsWrite) {
		t.Error("grant does not match the minted scopes")
	}
	if p.TokenID() != tok.ID || p.Name != "ci" {
		t.Errorf("principal identity = %d/%q, want %d/%q", p.TokenID(), p.Name, tok.ID, "ci")
	}
}

func TestAuthenticateUnknownRevokedAndExpiredAreIndistinguishable(t *testing.T) {
	st := newFakeTokens()
	m := NewManager(st, nil).withNow(func() time.Time { return testNow })
	ctx := workspace.With(context.Background(), "acme")

	revokedRaw, revokedTok, err := m.Mint(ctx, "gone", []Scope{ScopeMonitorsRead}, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := st.RevokeAPIToken(ctx, revokedTok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	expiredRaw, _, err := m.Mint(ctx, "old", []Scope{ScopeMonitorsRead}, testNow.Add(-time.Second))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	for name, candidate := range map[string]string{
		"unknown": "sb_not-a-token-at-all",
		"revoked": revokedRaw,
		"expired": expiredRaw,
		"empty":   "",
		"static":  "not-prefixed",
	} {
		_, err := m.Authenticate(ctx, candidate)
		// One error value and one error string for all five: which of them it was
		// is exactly what a caller could otherwise probe for.
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s: err = %v, want ErrInvalidToken", name, err)
		}
		if err.Error() != ErrInvalidToken.Error() {
			t.Errorf("%s: error text %q differs from the others", name, err)
		}
	}
}

func TestAuthenticateIsByDigestNotByRequestedWorkspace(t *testing.T) {
	// The lookup runs before tenancy is known, so a token authenticates into its
	// own workspace whoever the request thought it was calling as. A caller
	// cannot ask for another tenant's data with it.
	raw, _, m, _ := minted(t, ScopeMonitorsRead)
	p, err := m.Authenticate(workspace.With(context.Background(), "other-tenant"), raw)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Workspace != "acme" {
		t.Fatalf("workspace = %q, want acme", p.Workspace)
	}
}

func TestAuthenticateRecordsLastUsedOnceAMinute(t *testing.T) {
	clock := testNow
	st := newFakeTokens()
	m := NewManager(st, nil).withNow(func() time.Time { return clock })
	ctx := workspace.With(context.Background(), "acme")
	raw, _, err := m.Mint(ctx, "busy", []Scope{ScopeMonitorsRead}, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := m.Authenticate(ctx, raw); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(st.touches) != 1 {
		t.Fatalf("touches = %v, want the first use recorded", st.touches)
	}

	// Twenty requests in the same minute write the row once. The stamp answers
	// "is this token stale?", and "used in the last minute" answers that just as
	// well as "used 40ms ago" — at a twentieth of the write load.
	clock = testNow.Add(30 * time.Second)
	for range 20 {
		if _, err := m.Authenticate(ctx, raw); err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
	}
	if len(st.touches) != 1 {
		t.Fatalf("touches = %d, want throttled to one per minute", len(st.touches))
	}

	clock = testNow.Add(2 * time.Minute)
	if _, err := m.Authenticate(ctx, raw); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(st.touches) != 2 {
		t.Fatalf("touches = %d, want a fresh one after the interval", len(st.touches))
	}
}

func TestAuthenticateSurvivesATouchFailure(t *testing.T) {
	// A credential whose last-used stamp could not be written is still valid.
	// Failing the request would turn a storage hiccup into an outage.
	st := newFakeTokens()
	st.touchErr = errors.New("database is busy")
	m := NewManager(st, nil)
	ctx := workspace.With(context.Background(), "acme")
	raw, _, err := m.Mint(ctx, "ci", []Scope{ScopeMonitorsRead}, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.Authenticate(ctx, raw); err != nil {
		t.Fatalf("Authenticate with a failing touch = %v, want success", err)
	}
}

func TestPrincipalAllowed(t *testing.T) {
	scoped := &Principal{restricted: true, scopes: []Scope{ScopeMonitorsRead}}
	if scoped.Allowed(ScopeMonitorsWrite) {
		t.Error("read must not imply write")
	}
	writer := &Principal{restricted: true, scopes: []Scope{ScopeMonitorsWrite}}
	if !writer.Allowed(ScopeMonitorsRead) {
		t.Error("write must carry read of the same resource")
	}
	if writer.Allowed(ScopeChannelsRead) {
		t.Error("the implication is inside one resource, not across resources")
	}
	// ScopeNone is a route asking for no permission, not a route asking for
	// nothing in particular: any valid credential passes it.
	if !scoped.Allowed(ScopeNone) {
		t.Error("ScopeNone must be allowed for a restricted caller")
	}
	var nilP *Principal
	if nilP.Allowed(ScopeNone) {
		t.Error("no caller has no permission, not even on a free route")
	}
	if nilP.Unrestricted() {
		t.Error("a nil principal must not read as an unrestricted caller")
	}

	// The round trip that motivated the `restricted` flag: a token minted with
	// no scopes stores an empty list, reads back as an empty list, and must not
	// become the all-scopes credential.
	st := newFakeTokens()
	m := NewManager(st, nil)
	raw, _, err := m.Mint(workspace.With(context.Background(), "acme"), "empty", nil, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	p, err := m.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Unrestricted() || p.Allowed(ScopeMonitorsRead) {
		t.Fatal("a scopeless token must be able to call nothing, not everything")
	}

	star := &Principal{restricted: true, scopes: []Scope{ScopeAll}}
	if !star.Allowed(ScopeChannelsWrite) || !star.Allowed(ScopeTokensWrite) {
		t.Error("* carries every scope")
	}
}

func TestCanGrant(t *testing.T) {
	operator := &Principal{Workspace: workspace.Default}
	if !operator.CanGrant([]Scope{ScopeAll}) {
		t.Error("an unrestricted caller can grant anything — that is API_TOKEN's reach")
	}

	monitors := &Principal{restricted: true, scopes: []Scope{ScopeMonitorsRead, ScopeMonitorsWrite}}
	if !monitors.CanGrant([]Scope{ScopeMonitorsRead}) {
		t.Error("a token can mint what it already holds")
	}
	if !monitors.CanGrant(nil) {
		t.Error("granting no scopes asks for nothing and is allowed")
	}
	// The escalation this closes: tokens:write must not be a road to full
	// control of the instance.
	for _, want := range [][]Scope{
		{ScopeChannelsWrite},
		{ScopeMonitorsRead, ScopeChannelsWrite},
		{ScopeAll},
	} {
		if monitors.CanGrant(want) {
			t.Errorf("CanGrant(%v) = true, want false", want)
		}
	}

	// "*" is not a scope a restricted token can pass on, even one holding "*" —
	// it includes scopes that do not exist yet.
	star := &Principal{restricted: true, scopes: []Scope{ScopeAll}}
	if star.CanGrant([]Scope{ScopeAll}) {
		t.Error("a restricted token may not mint a * token")
	}
	if !star.CanGrant([]Scope{ScopeChannelsWrite}) {
		t.Error("* holds every concrete scope, so it may mint one")
	}
	var none *Principal
	if none.CanGrant([]Scope{ScopeNone}) {
		t.Error("no caller grants nothing")
	}
}

func TestHashToken(t *testing.T) {
	// Deterministic, because authentication is an equality lookup, and not the
	// input, because the table must not be a list of tokens. Two digests are
	// taken from two calls so the comparison below is between values rather
	// than between identical expressions.
	stable, again := HashToken("abc"), HashToken("abc")
	if stable != again {
		t.Fatal("digest must be stable")
	}
	if changed := HashToken("abd"); stable == changed {
		t.Fatal("digest must change with the input")
	}
	if stable == "abc" || len(stable) != 64 || strings.ToLower(stable) != stable {
		t.Errorf("digest %q is not a hex sha256", stable)
	}
}

func TestIsTokenBearer(t *testing.T) {
	if !IsTokenBearer(TokenPrefix + "anything") {
		t.Error("an sb_ credential is a token bearer")
	}
	// The prefix is what stops the two credential kinds shadowing each other: a
	// static token never queries the token table, and a token row is never
	// compared against the static list.
	if IsTokenBearer("hex-token-from-env") || IsTokenBearer("") {
		t.Error("a static token is not a database token")
	}
}

func TestManagerWithoutStorageFailsClosed(t *testing.T) {
	if _, err := NewManager(nil, nil).Authenticate(context.Background(), TokenPrefix+"x"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
	var m *Manager
	if _, err := m.Authenticate(context.Background(), "x"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("nil manager: err = %v", err)
	}
	if err := m.Revoke(context.Background(), 1); err == nil {
		t.Error("a nil manager cannot revoke")
	}
	if toks, err := m.List(context.Background()); err != nil || toks != nil {
		t.Errorf("a nil manager List = %v, %v", toks, err)
	}
}

func TestRevokePassesThroughTheCallerWorkspace(t *testing.T) {
	st := newFakeTokens()
	m := NewManager(st, nil)
	_, tok, err := m.Mint(workspace.With(context.Background(), "acme"), "ci", []Scope{ScopeMonitorsRead}, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Another tenant gets the store's not-found, which the API turns into the
	// same 404 as a nonexistent id.
	if err := m.Revoke(workspace.With(context.Background(), "beta"), tok.ID); err == nil {
		t.Error("revoking across tenants must not succeed")
	}
	if err := m.Revoke(workspace.With(context.Background(), "acme"), tok.ID); err != nil {
		t.Errorf("revoking in the owning tenant: %v", err)
	}
}

// TestPrincipalChecksEachCredentialKindOnce pins the order that decides which
// store a bearer string is offered to: an sb_-shaped token goes to the token
// table and nowhere else, anything else goes to the static list.
func TestPrincipalChecksEachCredentialKindOnce(t *testing.T) {
	st := newFakeTokens()
	m := NewManager(st, nil)
	// "sb_" on the static side on purpose: a scoped-looking string that the
	// table does not know must still be refused, so a revoked or unknown token
	// cannot be revived by also configuring it as API_TOKEN.
	a := NewBound([]Binding{{Workspace: "acme", Token: "static-acme"}, {Workspace: "beta", Token: TokenPrefix + "configured-by-hand"}}, 0).WithTokens(m)

	raw, _, err := m.Mint(workspace.With(context.Background(), "acme"), "ci", []Scope{ScopeMonitorsRead}, time.Time{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	for _, tc := range []struct {
		name       string
		bearer     string
		wantOK     bool
		wantWS     workspace.ID
		restricted bool
	}{
		{"static token", "static-acme", true, "acme", false},
		{"scoped token", raw, true, "acme", true},
		{"scoped-looking but unknown", TokenPrefix + "guess", false, "", false},
		{"scoped-looking and configured statically", TokenPrefix + "configured-by-hand", false, "", false},
		{"wrong credential", "nope", false, "", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
		req.Header.Set("Authorization", "Bearer "+tc.bearer)
		p, ok := a.Principal(req)
		if ok != tc.wantOK {
			t.Fatalf("%s: ok = %v, want %v", tc.name, ok, tc.wantOK)
		}
		if !tc.wantOK {
			continue
		}
		if p.Workspace != tc.wantWS {
			t.Errorf("%s: workspace = %q, want %q", tc.name, p.Workspace, tc.wantWS)
		}
		if (p.restricted) != tc.restricted {
			t.Errorf("%s: restricted = %v, want %v", tc.name, p.restricted, tc.restricted)
		}
	}

	// A session is an unrestricted principal: cookies cannot carry the scopes of
	// the credential that signed in, which is why Login refuses a scoped token.
	id, ok := a.Login("static-acme")
	if !ok {
		t.Fatal("a static token must still sign in")
	}
	if _, ok := a.Login(raw); ok {
		t.Fatal("a scoped token must not mint a dashboard session")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts.csv", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: id})
	p, ok := a.Principal(req)
	if !ok {
		t.Fatal("a live session authenticates")
	}
	if !p.Unrestricted() || p.Workspace != "acme" {
		t.Errorf("session principal = %+v", p)
	}
}

func TestPrincipalContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := PrincipalFromContext(ctx); ok {
		t.Fatal("a request that never passed the middleware has no principal")
	}
	p := &Principal{Workspace: "acme", restricted: true, scopes: []Scope{ScopeStatsRead}}
	got, ok := PrincipalFromContext(WithPrincipal(ctx, p))
	if !ok || got != p {
		t.Fatalf("PrincipalFromContext = %v, %v", got, ok)
	}
	// A nil principal in the context is "no caller", never "unrestricted".
	if _, ok := PrincipalFromContext(WithPrincipal(ctx, nil)); ok {
		t.Error("a nil principal must not read as a caller")
	}
}
