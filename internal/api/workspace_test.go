package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const (
	acmeToken = "tok-acme-0123456789"
	betaToken = "tok-beta-0123456789"
)

// twoWorkspaces builds the authenticator both backends of this test present:
// one credential per workspace, which is the only way a request can be scoped.
func twoWorkspaces() *auth.Authenticator {
	return auth.NewBound([]auth.Binding{
		{Workspace: workspace.ID("acme"), Token: acmeToken},
		{Workspace: workspace.ID("beta"), Token: betaToken},
	}, 0)
}

// tenantServer builds the real API over a real SQLite store with two
// workspaces, each owning one monitor. Middleware and store both run, so an
// isolation bug in either shows up here as a monitor the other tenant can see.
func tenantServer(t *testing.T) chi.Router {
	t.Helper()
	return authServerOn(newTenantStore(t), twoWorkspaces())
}

func newTenantStore(t *testing.T) store.Store {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	if err := store.Migrate(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.NewSQLite(context.Background(), url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	for _, ws := range []workspace.ID{"acme", "beta"} {
		m := &store.Monitor{
			Name:        string(ws) + " monitor",
			ContractIDs: []string{validContract},
			Enabled:     true,
			Priority:    store.PriorityNormal,
		}
		if err := st.CreateMonitor(workspace.With(context.Background(), ws), m); err != nil {
			t.Fatalf("seed %s: %v", ws, err)
		}
	}
	return st
}

// listedNames returns the monitor names one GET /monitors response holds, and
// fails the test unless the call itself succeeded.
func listedNames(t *testing.T, res *http.Response) []string {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Monitors []store.Monitor `json:"monitors"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make([]string, 0, len(body.Monitors))
	for _, m := range body.Monitors {
		out = append(out, m.Name)
	}
	return out
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// TestOneTokenCannotReadAnotherWorkspacesMonitors is the end-to-end form of the
// tenancy contract: the credential picks the workspace, the store applies it,
// and a listing over HTTP returns one tenant's rows and nothing else.
func TestOneTokenCannotReadAnotherWorkspacesMonitors(t *testing.T) {
	h := tenantServer(t)

	for _, tc := range []struct{ token, want string }{
		{acmeToken, "acme monitor"},
		{betaToken, "beta monitor"},
	} {
		got := listedNames(t, get(t, h, "/monitors", bearer(tc.token)))
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("listing with %s = %v, want exactly [%s]", tc.token[:8]+"…", got, tc.want)
		}
	}
}

// TestWorkspaceHeaderCannotSteerTheScope pins the decision that the scope is a
// property of the credential and of nothing the client sends.
func TestWorkspaceHeaderCannotSteerTheScope(t *testing.T) {
	h := tenantServer(t)

	headers := bearer(acmeToken)
	headers["X-Workspace"] = "beta"
	if got := listedNames(t, get(t, h, "/monitors", headers)); len(got) != 1 || got[0] != "acme monitor" {
		t.Fatalf("X-Workspace moved the scope: %v", got)
	}

	// A query parameter is the same attack in a form that survives being pasted
	// into an address bar, so it gets the same answer.
	if got := listedNames(t, get(t, h, "/monitors?workspace=beta", bearer(acmeToken))); len(got) != 1 || got[0] != "acme monitor" {
		t.Fatalf("?workspace moved the scope: %v", got)
	}
}

// TestWritesLandInTheCallersWorkspace checks the other half of isolation: a
// monitor created with beta's credential must not become visible to acme, and
// the id beta just received must address nothing on acme's side.
func TestWritesLandInTheCallersWorkspace(t *testing.T) {
	h := tenantServer(t)

	req := httptest.NewRequest(http.MethodPost, "/monitors",
		strings.NewReader(`{"name":"shared name","contract_ids":["`+validContract+`"]}`))
	req.Header.Set("Authorization", "Bearer "+betaToken)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("create as beta = %d, want 201: %s", res.Code, res.Body.String())
	}
	var made store.Monitor
	if err := json.Unmarshal(res.Body.Bytes(), &made); err != nil {
		t.Fatalf("decode created monitor: %v", err)
	}
	if made.ID == 0 {
		t.Fatal("created monitor came back without an id")
	}

	if got := listedNames(t, get(t, h, "/monitors", bearer(acmeToken))); len(got) != 1 || got[0] != "acme monitor" {
		t.Fatalf("beta's write appeared for acme: %v", got)
	}
	if got := listedNames(t, get(t, h, "/monitors", bearer(betaToken))); len(got) != 2 {
		t.Fatalf("beta should own both of its monitors, got %v", got)
	}

	// An id-addressed read is the shortest path to another tenant's row, since
	// the id is a genuine one beta just received.
	foreign := get(t, h, "/monitors/"+strconv.FormatInt(made.ID, 10), bearer(acmeToken))
	defer func() { _ = foreign.Body.Close() }()
	if foreign.StatusCode != http.StatusNotFound {
		t.Fatalf("acme reading beta's monitor by id = %d, want 404", foreign.StatusCode)
	}
}

// TestSessionCookieKeepsItsTokensWorkspace covers the dashboard path: the scope
// must survive the exchange of a token for a cookie, or signing in as one
// tenant would silently grant another workspace's data.
func TestSessionCookieKeepsItsTokensWorkspace(t *testing.T) {
	a := twoWorkspaces()
	id, ok := a.Login(betaToken)
	if !ok {
		t.Fatal("login with a configured workspace token failed")
	}
	h := authServerOn(newTenantStore(t), a)

	req := httptest.NewRequest(http.MethodGet, "/monitors", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: id})
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("session cookie = %d, want 200", res.Code)
	}
	var body struct {
		Monitors []store.Monitor `json:"monitors"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Monitors) != 1 || body.Monitors[0].Name != "beta monitor" {
		names := make([]string, 0, len(body.Monitors))
		for _, m := range body.Monitors {
			names = append(names, m.Name)
		}
		t.Fatalf("session scoped to %v, want exactly [beta monitor]", names)
	}
}

// TestUnknownWorkspaceFailsClosed guards the config side of the middleware: a
// credential whose workspace was dropped as invalid must not authenticate into
// the default workspace, which is what a silent fallback would mean.
func TestUnknownWorkspaceFailsClosed(t *testing.T) {
	a := auth.NewBound([]auth.Binding{
		{Workspace: workspace.ID("Acme"), Token: acmeToken}, // uppercase: invalid id, dropped
		{Workspace: workspace.ID("acme"), Token: betaToken},
	}, 0)
	h := authServerOn(newTenantStore(t), a)

	res := get(t, h, "/monitors", bearer(acmeToken))
	defer func() { _, _ = io.Copy(io.Discard, res.Body); _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("credential for an invalid workspace = %d, want 401", res.StatusCode)
	}
}
