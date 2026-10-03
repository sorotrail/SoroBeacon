package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const (
	acmeToken = "tok-acme-0123456789"
	betaToken = "tok-beta-0123456789"
)

// scopeRecorder keeps whatever workspace the handlers below it were scoped to.
// The store is where tenancy is applied, so what this records is what a
// dashboard request actually carries — a middleware that forgot to inject a
// scope would show up as an empty id, not as a passing test.
type scopeRecorder struct {
	emptyStore
	mu     sync.Mutex
	scope  workspace.ID
	sawAny bool
}

func (rec *scopeRecorder) ListMonitorsPage(ctx context.Context, _ store.ListFilter) ([]store.Monitor, error) {
	ws, _ := workspace.From(ctx)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.scope, rec.sawAny = ws, true
	return nil, nil
}

func (rec *scopeRecorder) recorded(t *testing.T) workspace.ID {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.sawAny {
		t.Fatal("the dashboard never reached a scoped store method, so nothing proved the injection")
	}
	return rec.scope
}

func scopedDashboard(t *testing.T, rec *scopeRecorder) http.Handler {
	t.Helper()
	a := auth.NewBound([]auth.Binding{
		{Workspace: workspace.ID("acme"), Token: acmeToken},
		{Workspace: workspace.ID("beta"), Token: betaToken},
	}, 0)
	s, err := New(rec, rules.NewRegistry(), notify.DefaultFactory(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.WithAuth(a).Routes()
}

// TestDashboardSignInScopesTheSessionToItsToken is the dashboard half of
// tenancy: the token decides the workspace once, at sign-in, and every page
// thereafter runs inside it.
func TestDashboardSignInScopesTheSessionToItsToken(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  workspace.ID
	}{
		{acmeToken, "acme"},
		{betaToken, "beta"},
	} {
		rec := &scopeRecorder{}
		h := scopedDashboard(t, rec)

		res := signIn(t, h, tc.token, "/monitors")
		cookie := sessionCookieOf(t, res)
		if cookie == nil {
			t.Fatalf("sign-in with %s set no session cookie", tc.token)
		}
		if got := getPath(t, h, "/monitors", cookie); got.StatusCode != http.StatusOK {
			t.Fatalf("monitors page = %d, want 200", got.StatusCode)
		}
		if got := rec.recorded(t); got != tc.want {
			t.Fatalf("sign-in with %s served workspace %q, want %q", tc.token, got, tc.want)
		}
	}
}

// TestDashboardSessionCannotBeRePointedAtAnotherWorkspace covers the same rule
// from the client's side: the URL and the cookies a browser controls are not
// how a scope is chosen.
func TestDashboardSessionCannotBeRePointedAtAnotherWorkspace(t *testing.T) {
	rec := &scopeRecorder{}
	h := scopedDashboard(t, rec)

	cookie := sessionCookieOf(t, signIn(t, h, acmeToken, ""))
	if cookie == nil {
		t.Fatal("sign-in set no session cookie")
	}
	// The cookie is the credential; renaming what it points at is not possible
	// because nothing reads a workspace from the request.
	if got := getPath(t, h, "/monitors?workspace=beta", cookie); got.StatusCode != http.StatusOK {
		t.Fatalf("monitors page = %d, want 200", got.StatusCode)
	}
	if got := rec.recorded(t); got != workspace.ID("acme") {
		t.Fatalf("?workspace moved the dashboard scope to %q", got)
	}

	// A session id is random and unguessable; a made-up one is not a scope
	// either, and must not reach the store at all.
	forged := &scopeRecorder{}
	hf := scopedDashboard(t, forged)
	res := getPath(t, hf, "/monitors", &http.Cookie{Name: auth.SessionCookie, Value: "forged-session-id"})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("forged session = %d, want a redirect to sign-in", res.StatusCode)
	}
	if forged.sawAny {
		t.Fatal("an unauthenticated request reached the store")
	}
}
