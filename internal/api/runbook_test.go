package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSQLiteAPIServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "api_runbook.db")
	dsn := "sqlite://" + dbPath
	require.NoError(t, store.Migrate(dsn))
	st, err := store.New(context.Background(), dsn, store.PoolSettings{}, nil)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	return srv, st
}

func TestAPIRunbookAndGuidanceValidationAndInheritance(t *testing.T) {
	t.Parallel()

	srv, st := newSQLiteAPIServer(t)
	router := srv.Routes()

	// 1. Reject invalid runbook_url scheme on monitor create.
	badMonBody := `{"name":"Bad","contract_ids":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],"runbook_url":"javascript:alert(1)"}`
	req := httptest.NewRequest(http.MethodPost, "/monitors", bytes.NewBufferString(badMonBody))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// 2. Create monitor with default runbook_url and response_guidance.
	monBody := `{
		"name": "Vault Monitor",
		"contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
		"runbook_url": "https://runbooks.example.com/vault-default",
		"response_guidance": "Default vault triage steps."
	}`
	req = httptest.NewRequest(http.MethodPost, "/monitors", bytes.NewBufferString(monBody))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var createdMon store.Monitor
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createdMon))
	assert.Equal(t, "https://runbooks.example.com/vault-default", createdMon.RunbookURL)
	assert.Equal(t, "Default vault triage steps.", createdMon.ResponseGuidance)

	// 3. Reject invalid runbook_url on rule create.
	badRuleBody := `{"type":"event_emitted","params":{"event_name":"transfer"},"runbook_url":"ftp://bad.example.com"}`
	req = httptest.NewRequest(http.MethodPost, "/monitors/1/rules", bytes.NewBufferString(badRuleBody))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// 4. Create rule overriding response_guidance while leaving runbook_url to inherit from monitor.
	ruleBody := `{
		"type": "event_emitted",
		"params": {"event_name": "transfer"},
		"response_guidance": "Rule-level transfer response guidance."
	}`
	req = httptest.NewRequest(http.MethodPost, "/monitors/1/rules", bytes.NewBufferString(ruleBody))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var createdRule store.Rule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createdRule))
	assert.Equal(t, "", createdRule.RunbookURL)
	assert.Equal(t, "Rule-level transfer response guidance.", createdRule.ResponseGuidance)

	// 5. Update rule via PATCH to add runbook_url.
	patchBody := `{"runbook_url": "https://runbooks.example.com/rule-specific"}`
	req = httptest.NewRequest(http.MethodPatch, "/monitors/1/rules/1", bytes.NewBufferString(patchBody))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var updatedRule store.Rule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updatedRule))
	assert.Equal(t, "https://runbooks.example.com/rule-specific", updatedRule.RunbookURL)
	assert.Equal(t, "Rule-level transfer response guidance.", updatedRule.ResponseGuidance)

	// Clear rule's runbook_url so monitor fallback is tested on alert creation.
	clearPatch := `{"runbook_url": ""}`
	req = httptest.NewRequest(http.MethodPatch, "/monitors/1/rules/1", bytes.NewBufferString(clearPatch))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// 6. Create an alert in store and verify GET /alerts returns inherited runbook_url and rule guidance.
	alert := &store.Alert{
		MonitorID: createdMon.ID,
		RuleID:    createdRule.ID,
		EventID:   "ev-inherit-1",
		Payload:   json.RawMessage(`{"contract_id":"CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA","event_name":"transfer"}`),
	}
	outcome, err := st.CreateAlert(context.Background(), alert)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	req = httptest.NewRequest(http.MethodGet, "/alerts", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var page struct {
		Alerts []store.Alert `json:"alerts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Alerts, 1)
	assert.Equal(t, "https://runbooks.example.com/vault-default", page.Alerts[0].RunbookURL)
	assert.Equal(t, "Rule-level transfer response guidance.", page.Alerts[0].ResponseGuidance)
}
