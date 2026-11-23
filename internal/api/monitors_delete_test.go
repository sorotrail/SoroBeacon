package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// seedMonitorWithRule gives a deletion something to leave behind: a monitor
// row and one rule attached to it. Seeding through the store rather than the
// API keeps create-audit noise out of what the DELETE is expected to log.
func seedMonitorWithRule(t *testing.T, st store.Store) (*store.Monitor, *store.Rule) {
	t.Helper()
	ctx := context.Background()
	m := &store.Monitor{Name: "doomed", ContractIDs: []string{validContract}, Enabled: true}
	if err := st.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	r := &store.Rule{
		MonitorID: m.ID,
		Type:      "event_emitted",
		Params:    json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:   true,
	}
	if err := st.CreateRule(ctx, r); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	return m, r
}

// TestDeleteMonitorCascadesAndAudits covers the soft-delete path over the real
// router and a real store: 204 on success, the monitor is marked as deleted
// (but not removed), its rules remain, and exactly one audit row saying what
// was deleted and who deleted it.
func TestDeleteMonitorCascadesAndAudits(t *testing.T) {
	srv, st, _ := sqliteAPI(t)
	m, rule := seedMonitorWithRule(t, st)
	ctx := context.Background()
	item := srv.URL + "/monitors/" + strconv.FormatInt(m.ID, 10)

	res, _ := do(t, http.MethodDelete, item, "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", res.StatusCode)
	}

	// After soft-delete, the monitor still exists but is marked as deleted.
	// It is excluded from normal listings but can be retrieved by ID.
	gone, _ := do(t, http.MethodGet, item, "", nil)
	if gone.StatusCode != http.StatusOK {
		t.Fatalf("GET after soft-delete = %d, want 200", gone.StatusCode)
	}

	// The rule is NOT deleted on soft-delete; it comes back on restore.
	if _, err := st.GetRule(ctx, rule.ID); err != nil {
		t.Fatalf("rule after soft-delete: err = %v, want nil", err)
	}
	rules, err := st.ListRules(ctx, m.ID, false)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("monitor still has %d rules after soft-delete, want 1", len(rules))
	}

	// The monitor should have DeletedAt set.
	mAfter, err := st.GetMonitor(ctx, m.ID)
	if err != nil {
		t.Fatalf("get monitor after soft-delete: %v", err)
	}
	if mAfter.DeletedAt == nil {
		t.Fatal("monitor DeletedAt should be set after soft-delete")
	}

	entries, err := st.ListAuditEntries(ctx, store.AuditFilter{
		TargetType: "monitor",
		TargetID:   m.ID,
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want exactly 1: %+v", len(entries), entries)
	}
	if entries[0].Action != store.AuditActionDelete {
		t.Errorf("action = %q, want %q", entries[0].Action, store.AuditActionDelete)
	}
	if entries[0].Actor == "" {
		t.Error("audit entry has no actor: the request id must record who deleted")
	}
}

// TestDeleteMonitorUnknownID pins 404 for a well-formed id that addresses
// nothing, and that the failed attempt is recorded with Outcome=="failed" and
// StatusCode==404 so operators can distinguish failed attempts from succeeded
// deletions.
func TestDeleteMonitorUnknownID(t *testing.T) {
	srv, st, logBuf := sqliteAPI(t)

	res, _ := do(t, http.MethodDelete, srv.URL+"/monitors/999999", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Logf("Log output: %s", logBuf.String())
		t.Fatalf("DELETE unknown id = %d, want 404", res.StatusCode)
	}

	entries, err := st.ListAuditEntries(context.Background(), store.AuditFilter{
		TargetType: "monitor",
		TargetID:   999999,
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("failed delete wrote %d audit rows, want 1: %+v", len(entries), entries)
	}
	if entries[0].Outcome != store.AuditOutcomeFailed || entries[0].StatusCode != http.StatusNotFound {
		t.Errorf("audit outcome/status = %q/%d, want %q/404", entries[0].Outcome, entries[0].StatusCode, store.AuditOutcomeFailed)
	}
}

// TestDeleteMonitorMalformedID pins 400 for ids the path parser rejects.
// Each shape fails at a different layer of strconv — non-numeric segment,
// a fraction, an integer wider than int64 — so all must stop at the same
// gate instead of reaching the store as a parsed zero or a wildcard.
func TestDeleteMonitorMalformedID(t *testing.T) {
	srv, _, _ := sqliteAPI(t)

	for _, id := range []string{"abc", "1.5", "999999999999999999999"} {
		t.Run(id, func(t *testing.T) {
			res, _ := do(t, http.MethodDelete, srv.URL+"/monitors/"+id, "", nil)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("DELETE /monitors/%s = %d, want 400", id, res.StatusCode)
			}
		})
	}
}

// TestDeleteMonitorCannotReachAnotherWorkspace: holding a genuine id from
// another tenant must not help — the store scopes the DELETE by workspace,
// so the answer is 404 exactly like an unknown id (not 403, which would
// confirm the row exists), and the victim's monitor stays intact for its
// owner afterwards.
func TestDeleteMonitorCannotReachAnotherWorkspace(t *testing.T) {
	h := tenantServer(t)

	// beta creates a monitor and learns its id the legitimate way...
	req := httptest.NewRequest(http.MethodPost, "/monitors",
		strings.NewReader(`{"name":"beta keep","contract_ids":["`+validContract+`"]}`))
	req.Header.Set("Authorization", "Bearer "+betaToken)
	req.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, req)
	if created.Code != http.StatusCreated {
		t.Fatalf("beta create = %d, want 201: %s", created.Code, created.Body.String())
	}
	var m store.Monitor
	if err := json.Unmarshal(created.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode created monitor: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("created monitor came back without an id")
	}
	item := "/monitors/" + strconv.FormatInt(m.ID, 10)

	// ...acme deletes it with that id and must learn nothing from the answer.
	del := httptest.NewRequest(http.MethodDelete, item, nil)
	del.Header.Set("Authorization", "Bearer "+acmeToken)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, del)
	if res.Code != http.StatusNotFound {
		t.Fatalf("acme deleting beta's monitor = %d, want 404", res.Code)
	}

	// beta's monitor is untouched by the attempt.
	kept := get(t, h, item, bearer(betaToken))
	defer func() { _ = kept.Body.Close() }()
	if kept.StatusCode != http.StatusOK {
		t.Fatalf("beta reading its monitor after acme's delete = %d, want 200", kept.StatusCode)
	}
}
