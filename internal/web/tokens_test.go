package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// The dashboard's token page is where an operator reads the "shown once"
// promise, so these tests hold it: the secret appears on the mint response and
// on nothing else — not the listing, not the log, not a redirect.

// tokenStore is the api_tokens table for these tests: workspace-aware, newest
// first, keeping the row on revocation. Real enough to exercise the handlers'
// tenancy and state logic without a database.
type tokenStore struct {
	emptyStore
	mu    sync.Mutex
	rows  map[int64]*auth.Token
	next  int64
	nowFn func() time.Time
}

func newTokenStore() *tokenStore {
	return &tokenStore{
		rows:  map[int64]*auth.Token{},
		nowFn: func() time.Time { return time.Unix(1_800_000_000, 0).UTC() },
	}
}

func (s *tokenStore) CreateAPIToken(ctx context.Context, t *auth.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	t.ID = s.next
	t.Workspace = wsOf(ctx)
	t.CreatedAt = s.nowFn()
	stored := *t
	s.rows[stored.ID] = &stored
	return nil
}

func (s *tokenStore) TokenByHash(_ context.Context, hash string) (*auth.Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.rows {
		if t.Hash == hash {
			copied := *t
			return &copied, true, nil
		}
	}
	return nil, false, nil
}

func (s *tokenStore) ListAPITokens(ctx context.Context) ([]auth.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := wsOf(ctx)
	var out []auth.Token
	for _, t := range s.rows {
		if t.Workspace == ws {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (s *tokenStore) RevokeAPIToken(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.rows[id]
	if !ok || t.Workspace != wsOf(ctx) {
		return store.ErrNotFound
	}
	if t.RevokedAt.IsZero() {
		t.RevokedAt = s.nowFn()
	}
	return nil
}

func (s *tokenStore) TouchAPIToken(ctx context.Context, id int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.rows[id]; ok && t.Workspace == wsOf(ctx) {
		t.LastUsedAt = at
	}
	return nil
}

// seed stores one token directly, so a test can start from a row that already
// exists instead of from a mint it is also auditing.
func (s *tokenStore) seed(ws workspace.ID, name, secret string, scopes ...auth.Scope) *auth.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	tok := &auth.Token{
		ID:        s.next,
		Name:      name,
		Prefix:    secret[:len(auth.TokenPrefix)+8],
		Hash:      auth.HashToken(secret),
		Workspace: ws,
		Scopes:    scopes,
		CreatedAt: s.nowFn(),
	}
	s.rows[tok.ID] = tok
	return tok
}

func (s *tokenStore) byID(t *testing.T, id int64) auth.Token {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		t.Fatalf("no token with id %d", id)
	}
	return *row
}

func (s *tokenStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// only returns the single row, failing the test if there is not exactly one.
func (s *tokenStore) only(t *testing.T) auth.Token {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) != 1 {
		t.Fatalf("expected exactly one token row, found %d", len(s.rows))
	}
	for _, row := range s.rows {
		return *row
	}
	return auth.Token{}
}

func wsOf(ctx context.Context) workspace.ID {
	if ws, ok := workspace.From(ctx); ok {
		return ws
	}
	return workspace.Default
}

// tokenDashboard builds the dashboard over token storage. wired false is the
// state an instance with no token table is in, which the page still has to
// answer politely.
func tokenDashboard(t *testing.T, st *tokenStore, w io.Writer) (http.Handler, *slog.Logger) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(w, nil))
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.WithTokens(auth.NewManager(st, logger)).Routes(), logger
}

func getTokenPage(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res.Code, res.Body.String()
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res.Result()
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

const (
	defaultSecret = "sb_cisecret000000000000000000000000000000000000"
	otherSecret   = "sb_teamscret00000000000000000000000000000000000"
)

func TestTokensPageListsRowsWithoutSecrets(t *testing.T) {
	st := newTokenStore()
	st.seed(workspace.Default, "ci-deploy", defaultSecret, auth.ScopeMonitorsRead, auth.ScopeMonitorsWrite)
	st.seed("acme", "team-token", otherSecret, auth.ScopeAll)
	h, _ := tokenDashboard(t, st, io.Discard)

	code, page := getTokenPage(t, h, "/tokens")
	if code != http.StatusOK {
		t.Fatalf("GET /tokens = %d", code)
	}
	// Name and display prefix: the handle that lets an operator match a token
	// they found pasted somewhere to a row they can retire.
	if !strings.Contains(page, "ci-deploy") || !strings.Contains(page, "sb_cisecret") {
		t.Fatalf("the row should show its name and prefix:\n%s", page)
	}
	// A dashboard request with no workspace is the default tenant, so another
	// team's token is not on this page at all.
	if strings.Contains(page, "team-token") {
		t.Errorf("the default workspace listed acme's token:\n%s", page)
	}
	// Neither the secret nor its digest belongs in the HTML.
	if strings.Contains(page, defaultSecret) || strings.Contains(page, auth.HashToken(defaultSecret)) {
		t.Errorf("the page leaked a credential")
	}
}

func TestDashboardMintsAndShowsTheSecretOnce(t *testing.T) {
	st := newTokenStore()
	h, _ := tokenDashboard(t, st, io.Discard)

	res := postForm(t, h, "/tokens", url.Values{
		"name":       {"nightly"},
		"scopes":     {"monitors:read", "alerts:read"},
		"expires_in": {"30"},
	})
	code, page := res.StatusCode, readBody(t, res)
	if code != http.StatusOK {
		t.Fatalf("POST /tokens = %d, want the same page rendered with the secret inline:\n%s", code, page)
	}

	row := st.only(t)
	if row.Name != "nightly" {
		t.Errorf("stored name = %q", row.Name)
	}
	if !slices.Equal(row.Scopes, []auth.Scope{auth.ScopeAlertsRead, auth.ScopeMonitorsRead}) {
		t.Errorf("stored scopes = %v, want the grant, sorted", row.Scopes)
	}
	// parseExpiry adds days to the real clock, not the store's, so the expected
	// expiry is measured against time.Now too; the minute of slack is the
	// difference between the two reads.
	if want := time.Now().AddDate(0, 0, 30); row.ExpiresAt.Before(want.Add(-time.Minute)) || row.ExpiresAt.After(want.Add(time.Minute)) {
		t.Errorf("expiry = %v, want about 30 days out from %v", row.ExpiresAt, want)
	}
	if row.Workspace != workspace.Default {
		t.Errorf("the row belongs to %q, want the requesting dashboard's tenant", row.Workspace)
	}

	// The mint page is the one place the secret exists in the UI. A page that
	// minted silently would produce credentials nobody can use.
	minted := mintedSecret(t, page)
	if !strings.HasPrefix(minted, auth.TokenPrefix) {
		t.Fatalf("no usable secret on the mint page:\n%s", page)
	}
	if got := st.only(t).Hash; got != auth.HashToken(minted) {
		t.Error("the stored row is not the digest of the secret that was shown")
	}
	// Inline, not via a redirect: a redirect would carry the secret in a URL,
	// which browsers, proxies and Referer headers all keep.
	if code == http.StatusSeeOther || code == http.StatusFound || res.Header.Get("Location") != "" {
		t.Errorf("the mint redirected to %q", res.Header.Get("Location"))
	}

	// Every later view has nothing left to reveal.
	if _, later := getTokenPage(t, h, "/tokens"); strings.Contains(later, minted) {
		t.Errorf("the listing showed the secret after the mint:\n%s", later)
	}
}

// mintedSecret pulls the shown-once token out of the mint page's copy box, which
// is the element the page promises is transient.
func mintedSecret(t *testing.T, page string) string {
	t.Helper()
	_, after, found := strings.Cut(page, `id="new_token">`)
	if !found {
		return ""
	}
	raw, _, _ := strings.Cut(after, "<")
	return strings.TrimSpace(raw)
}

func TestDashboardMintRejectsAFormThatCannotWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		form   url.Values
		wantIn string
	}{
		{"no scopes", url.Values{"name": {"x"}}, "at least one scope"},
		{"unknown scope", url.Values{"name": {"x"}, "scopes": {"monitors:see"}}, "unknown scope"},
		{"expiry that is not days", url.Values{"name": {"x"}, "scopes": {"monitors:read"}, "expires_in": {"0"}}, "whole number of days"},
	} {
		st := newTokenStore()
		h, _ := tokenDashboard(t, st, io.Discard)
		res := postForm(t, h, "/tokens", tc.form)
		code, page := res.StatusCode, readBody(t, res)
		if code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", tc.name, code)
		}
		if !strings.Contains(page, tc.wantIn) {
			t.Errorf("%s: the page does not say why the form was refused (want %q)", tc.name, tc.wantIn)
		}
		// A refused form must not have minted anything: the error page is the same
		// page, so a half-applied write would be easy to miss.
		if n := st.count(); n != 0 {
			t.Errorf("%s stored %d tokens, want none", tc.name, n)
		}
	}
}

func TestDashboardRevokeKeepsTheRowOnThePage(t *testing.T) {
	st := newTokenStore()
	tok := st.seed(workspace.Default, "retire-me", "sb_retiresecret0000000000000000000000000000", auth.ScopeMonitorsRead)
	h, _ := tokenDashboard(t, st, io.Discard)

	res := postForm(t, h, "/tokens/"+strconv.FormatInt(tok.ID, 10)+"/revoke", nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST revoke = %d, want 303 back to the page", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "/tokens" {
		t.Errorf("Location = %q, want /tokens", got)
	}
	_ = readBody(t, res)

	code, page := getTokenPage(t, h, "/tokens")
	if code != http.StatusOK {
		t.Fatalf("GET /tokens = %d", code)
	}
	// Revocation keeps the row: the listing still has to answer "which token was
	// that, and when was it last used".
	if !strings.Contains(page, "retire-me") || !strings.Contains(page, "revoked") {
		t.Errorf("a revoked token should stay listed and labelled:\n%s", page)
	}
	if st.byID(t, tok.ID).RevokedAt.IsZero() {
		t.Error("the row was not revoked")
	}
}

func TestDashboardRevokeDoesNotCrossWorkspaces(t *testing.T) {
	st := newTokenStore()
	tok := st.seed("acme", "team-token", otherSecret, auth.ScopeMonitorsRead)
	h, _ := tokenDashboard(t, st, io.Discard)

	// This dashboard request carries no workspace, so it acts as the default
	// tenant, and acme's id does not exist here — the same 404 a typo'd id gets.
	res := postForm(t, h, "/tokens/"+strconv.FormatInt(tok.ID, 10)+"/revoke", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-workspace revoke = %d, want 404", res.StatusCode)
	}
	if !st.byID(t, tok.ID).RevokedAt.IsZero() {
		t.Error("another workspace's token was revoked")
	}

	res = postForm(t, h, "/tokens/999999/revoke", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", res.StatusCode)
	}
}

func TestTokensPageWithoutTokenManagement(t *testing.T) {
	// An instance with no token storage answers the page instead of panicking, and
	// says what is missing rather than showing an empty table that reads as "no
	// tokens yet".
	st := newTokenStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := s.Routes()

	code, page := getTokenPage(t, h, "/tokens")
	if code != http.StatusOK {
		t.Fatalf("GET /tokens = %d, want the page with a notice", code)
	}
	if !strings.Contains(page, "not configured") {
		t.Errorf("expected a notice about token management, got:\n%s", page)
	}
	if res := postForm(t, h, "/tokens", url.Values{"name": {"x"}, "scopes": {"monitors:read"}}); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("mint with no storage = %d, want 503", res.StatusCode)
	}
	if res := postForm(t, h, "/tokens/1/revoke", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("revoke with no storage = %d, want 404", res.StatusCode)
	}
}

func TestMintLogLineNamesTheTokenWithoutContainingIt(t *testing.T) {
	// The log is the one place an attacker can read without authenticating, so the
	// creation line identifies a credential by id and label only.
	var logs strings.Builder
	st := newTokenStore()
	h, _ := tokenDashboard(t, st, &logs)

	res := postForm(t, h, "/tokens", url.Values{"name": {"logged"}, "scopes": {"monitors:read"}})
	page := readBody(t, res)
	minted := mintedSecret(t, page)
	if minted == "" {
		t.Fatal("the mint page showed no secret")
	}

	line := logs.String()
	if strings.Contains(line, minted) || strings.Contains(line, auth.HashToken(minted)) {
		t.Errorf("the log leaked the credential:\n%s", line)
	}
	if !strings.Contains(line, "api token created") || !strings.Contains(line, "logged") ||
		!strings.Contains(line, "monitors:read") {
		t.Errorf("expected a creation line naming the token, its label and its scopes:\n%s", line)
	}
}

func TestParseExpiry(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr bool
		never   bool
	}{
		{"", false, true},
		{"never", false, true},
		{" 30 ", false, false},
		{"1", false, false},
		{"3650", false, false},
		{"0", true, false},
		{"-5", true, false},
		{"3651", true, false},
		{"72h", true, false},
		{"forever", true, false},
	} {
		got, err := parseExpiry(tc.in)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("parseExpiry(%q) = %v, want an error", tc.in, got)
		case tc.wantErr:
		case err != nil:
			t.Errorf("parseExpiry(%q): %v", tc.in, err)
		case tc.never && !got.IsZero():
			t.Errorf("parseExpiry(%q) = %v, want the zero time", tc.in, got)
		case !tc.never && got.Before(time.Now()):
			t.Errorf("parseExpiry(%q) = %v, want the future", tc.in, got)
		}
	}
}

func TestTokenStateLabels(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, tc := range []struct {
		name string
		tok  auth.Token
		want string
	}{
		{"no expiry", auth.Token{}, "no expiry"},
		{"live with expiry", auth.Token{ExpiresAt: now.Add(time.Hour)}, "active"},
		{"expired", auth.Token{ExpiresAt: now.Add(-time.Hour)}, "expired"},
		{"expiring exactly now", auth.Token{ExpiresAt: now}, "expired"},
		{"revoked outranks expiry", auth.Token{RevokedAt: now, ExpiresAt: now.Add(-time.Hour)}, "revoked"},
		{"revoked with no expiry", auth.Token{RevokedAt: now}, "revoked"},
	} {
		if got := tokenState(tc.tok, now); got != tc.want {
			t.Errorf("%s: tokenState = %q, want %q", tc.name, got, tc.want)
		}
	}
}
