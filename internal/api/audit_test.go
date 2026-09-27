package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// auditFakeStore records the entries the middleware writes and serves a
// canned list, without needing a database.
type auditFakeStore struct {
	store.Store
	created    []*store.AuditEntry
	entries    []store.AuditEntry
	listErr    error
	lastFilter store.AuditFilter
}

func (f *auditFakeStore) CreateAuditEntry(_ context.Context, e *store.AuditEntry) error {
	e.ID = int64(len(f.created) + 1)
	f.created = append(f.created, e)
	return nil
}

func (f *auditFakeStore) ListAuditEntries(_ context.Context, fl store.AuditFilter) ([]store.AuditEntry, error) {
	f.lastFilter = fl
	return f.entries, f.listErr
}

func auditTestRouter(fs *auditFakeStore) chi.Router {
	r := chi.NewRouter()
	r.Use(AuditMiddleware(fs, discardLogger()))
	r.Route("/channels", func(r chi.Router) {
		r.Post("/", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusCreated, map[string]any{"id": 7})
		})
		r.Patch("/{id}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id")})
		})
		r.Delete("/{id}", func(w http.ResponseWriter, _ *http.Request) {
			writeNoContent(w)
		})
		// Not a configuration change: must not be audited.
		r.Post("/{id}/test", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "sent"})
		})
	})
	r.Route("/monitors", func(r chi.Router) {
		r.Post("/", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusCreated, map[string]any{"id": 3})
		})
		r.Route("/{id}/rules", func(r chi.Router) {
			r.Post("/", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusCreated, map[string]any{"id": 9})
			})
		})
	})
	return r
}

// secretConfig is the kind of body a channel write carries. Its value must
// never appear in an audit entry.
const secretConfig = `{"name":"ops","config":{"webhook_url":"https://hooks.example/SUPER-SECRET-TOKEN"}}`

func TestAuditMiddlewareRecordsOneEntryPerMutation(t *testing.T) {
	fs := &auditFakeStore{}
	r := auditTestRouter(fs)

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/channels/", secretConfig); rec.Code != http.StatusCreated {
		t.Fatalf("create channel = %d", rec.Code)
	}
	if rec := post("/monitors/", `{"name":"m"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create monitor = %d", rec.Code)
	}
	if rec := post("/monitors/3/rules/", `{"type":"event_emitted"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create rule = %d", rec.Code)
	}
	// A channel test-send is not a config change.
	post("/channels/1/test", `{}`)

	req := httptest.NewRequest(http.MethodPatch, "/channels/1", strings.NewReader(`{"config":{"webhook_url":"x"}}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update channel = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/channels/1", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete channel = %d", rec.Code)
	}

	if len(fs.created) != 5 {
		t.Fatalf("recorded %d entries, want 5: %+v", len(fs.created), fs.created)
	}
	want := []struct {
		action, target string
		id             int64
	}{
		{store.AuditActionCreate, "channel", 7},
		{store.AuditActionCreate, "monitor", 3},
		{store.AuditActionCreate, "rule", 9},
		{store.AuditActionUpdate, "channel", 1},
		{store.AuditActionDelete, "channel", 1},
	}
	for i, w := range want {
		got := fs.created[i]
		if got.Action != w.action || got.TargetType != w.target || got.TargetID != w.id {
			t.Errorf("entry %d = (%s %s %d), want (%s %s %d)",
				i, got.Action, got.TargetType, got.TargetID, w.action, w.target, w.id)
		}
		if got.Actor == "" {
			t.Errorf("entry %d has no actor (request id)", i)
		}
	}
	// The secret in the request body must never reach the log.
	for _, e := range fs.created {
		if strings.Contains(string(e.Diff), "SUPER-SECRET-TOKEN") {
			t.Fatalf("audit diff leaked a secret: %s", e.Diff)
		}
	}
	// The create entry records the field names, not their values.
	if !strings.Contains(string(fs.created[0].Diff), "config") {
		t.Errorf("create diff = %s, want the config field name recorded", fs.created[0].Diff)
	}
}

func TestListAudit(t *testing.T) {
	fs := &auditFakeStore{entries: []store.AuditEntry{{ID: 2, Action: store.AuditActionCreate, TargetType: "channel"}}}
	s := &Server{store: fs, log: discardLogger()}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/audit?target_type=channel&target_id=3&limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /audit = %d", res.StatusCode)
	}
	if fs.lastFilter.TargetType != "channel" || fs.lastFilter.TargetID != 3 || fs.lastFilter.Limit != 5 {
		t.Fatalf("filter = %+v", fs.lastFilter)
	}
}

func TestListAuditRejectsBadFilters(t *testing.T) {
	s := &Server{store: &auditFakeStore{}, log: discardLogger()}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	for _, q := range []string{"target_type=alert", "target_id=x", "from=yesterday", "to=nope", "limit=0"} {
		res, err := http.Get(srv.URL + "/audit?" + q)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /audit?%s = %d, want 400", q, res.StatusCode)
		}
	}
}
