package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSQLiteWebServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "web_runbook.db")
	dsn := "sqlite://" + dbPath
	require.NoError(t, store.Migrate(dsn))
	st, err := store.New(context.Background(), dsn, store.PoolSettings{}, nil)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), logger)
	require.NoError(t, err)
	return srv, st
}

func TestWebDashboardRunbookAndGuidanceFlow(t *testing.T) {
	t.Parallel()

	srv, st := newSQLiteWebServer(t)
	router := srv.Routes()

	// 1. Create monitor with runbook_url & response_guidance via web form.
	form := url.Values{
		"name":              {"Core Pool Monitor"},
		"contract_ids":      {"CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"},
		"runbook_url":       {"https://runbooks.example.com/pool-default"},
		"response_guidance": {"Monitor-level pool guidance."},
	}
	req := httptest.NewRequest(http.MethodPost, "/monitors", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code)

	// 2. Reject invalid runbook_url in rule builder and re-render error on monitor page.
	badRuleForm := url.Values{
		"type":              {"event_emitted"},
		"use_builder":       {"true"},
		"param_event_name":  {"swap"},
		"runbook_url":       {"javascript:alert(1)"},
		"response_guidance": {"Swap guidance"},
	}
	req = httptest.NewRequest(http.MethodPost, "/monitors/1/rules", strings.NewReader(badRuleForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "runbook_url")

	// 3. Create rule with response_guidance via rule builder.
	goodRuleForm := url.Values{
		"type":              {"event_emitted"},
		"use_builder":       {"true"},
		"param_event_name":  {"swap"},
		"response_guidance": {"Inspect pool reserves and pause swaps if invariant broke."},
	}
	req = httptest.NewRequest(http.MethodPost, "/monitors/1/rules", strings.NewReader(goodRuleForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code)

	// 4. Render monitor detail page and verify monitor runbook and rule guidance appear.
	req = httptest.NewRequest(http.MethodGet, "/monitors/1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `href="https://runbooks.example.com/pool-default"`)
	assert.Contains(t, body, "Inspect pool reserves and pause swaps if invariant broke.")

	// 5. Create alert and verify /alerts/1 renders inherited runbook link and rule guidance.
	alert := &store.Alert{
		MonitorID: 1,
		RuleID:    1,
		EventID:   "0002-1",
		Payload:   json.RawMessage(`{"contract_id":"CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA","event_name":"swap","ledger":100}`),
	}
	outcome, err := st.CreateAlert(context.Background(), alert)
	require.NoError(t, err)
	require.Equal(t, store.AlertCreated, outcome)

	req = httptest.NewRequest(http.MethodGet, "/alerts/1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	alertHTML := rec.Body.String()
	assert.Contains(t, alertHTML, `<th>Runbook</th><td><a href="https://runbooks.example.com/pool-default" rel="noopener noreferrer">https://runbooks.example.com/pool-default</a></td>`)
	assert.Contains(t, alertHTML, `<th>Guidance</th><td>Inspect pool reserves and pause swaps if invariant broke.</td>`)
}
