package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type taxonomyStore struct {
	store.Store
	err error
}

func (s *taxonomyStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	return nil, s.err
}

func (s *taxonomyStore) CreateMonitor(context.Context, *store.Monitor) error {
	return s.err
}

func (s *taxonomyStore) DeleteChannel(context.Context, int64) error {
	return s.err
}

func (s *taxonomyStore) CreateAuditEntry(context.Context, *store.AuditEntry) error {
	return nil
}

func executeRequest(t *testing.T, st store.Store, method, path string, body []byte) (int, map[string]any) {
	t.Helper()
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader([]byte{})
	}
	req, err := http.NewRequest(method, srv.URL+path, reqBody)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	var env map[string]any
	_ = json.NewDecoder(res.Body).Decode(&env)
	return res.StatusCode, env
}

func TestErrorTaxonomy_TriggersAndEnvelopes(t *testing.T) {
	t.Run("NotFound trigger", func(t *testing.T) {
		status, env := executeRequest(t, &taxonomyStore{err: store.ErrNotFound}, http.MethodGet, "/monitors/123", nil)
		assert.Equal(t, http.StatusNotFound, status)
		assert.Equal(t, apierr.CodeNotFound, env["code"])
		assert.Equal(t, "not found", env["error"])
		assert.NotEmpty(t, env["request_id"])
	})

	t.Run("AlreadyExists unique constraint trigger", func(t *testing.T) {
		uniqueErr := &store.ConstraintError{Kind: store.ConstraintUnique, Constraint: "monitors_name_key"}
		body := []byte(`{"name":"test","contract_ids":["` + validContract + `"]}`)
		status, env := executeRequest(t, &taxonomyStore{err: uniqueErr}, http.MethodPost, "/monitors", body)
		assert.Equal(t, http.StatusUnprocessableEntity, status)
		assert.Equal(t, apierr.CodeAlreadyExists, env["code"])
		assert.Equal(t, "the request conflicts with an existing record", env["error"])
	})

	t.Run("ForeignKey constraint trigger maps to NotFound code", func(t *testing.T) {
		fkErr := &store.ConstraintError{Kind: store.ConstraintForeignKey, Constraint: "rules_monitor_id_fkey"}
		body := []byte(`{"name":"test","contract_ids":["` + validContract + `"]}`)
		status, env := executeRequest(t, &taxonomyStore{err: fkErr}, http.MethodPost, "/monitors", body)
		assert.Equal(t, http.StatusUnprocessableEntity, status)
		assert.Equal(t, apierr.CodeNotFound, env["code"])
		assert.Equal(t, "the request references a record that does not exist", env["error"])
	})

	t.Run("Check constraint trigger maps to InvalidArgument code", func(t *testing.T) {
		checkErr := &store.ConstraintError{Kind: store.ConstraintCheck, Constraint: "channels_type_check"}
		body := []byte(`{"name":"test","contract_ids":["` + validContract + `"]}`)
		status, env := executeRequest(t, &taxonomyStore{err: checkErr}, http.MethodPost, "/monitors", body)
		assert.Equal(t, http.StatusUnprocessableEntity, status)
		assert.Equal(t, apierr.CodeInvalidArgument, env["code"])
		assert.Equal(t, "the request contains a value the database refuses", env["error"])
	})

	t.Run("Conflict sentinel trigger", func(t *testing.T) {
		status, env := executeRequest(t, &taxonomyStore{err: store.ErrChannelInUse}, http.MethodDelete, "/channels/5", nil)
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, apierr.CodeConflict, env["code"])
		assert.Equal(t, "channel is referenced by an escalation policy", env["error"])
	})

	t.Run("Unmapped internal database error never leaks internals", func(t *testing.T) {
		secretDBErr := errors.New("pq: password authentication failed for user 'secret_db_admin' on host 'db-prod-1.internal'")
		status, env := executeRequest(t, &taxonomyStore{err: secretDBErr}, http.MethodGet, "/monitors/456", nil)
		assert.Equal(t, http.StatusInternalServerError, status)
		assert.Equal(t, apierr.CodeInternalError, env["code"])
		assert.Equal(t, "internal error", env["error"])
		// Ensure raw error message, internal hostname, credentials, or table names never leak:
		for _, v := range env {
			str, ok := v.(string)
			if ok {
				assert.False(t, strings.Contains(str, "secret_db_admin"), "leaked secret user")
				assert.False(t, strings.Contains(str, "db-prod-1"), "leaked internal host")
				assert.False(t, strings.Contains(str, "password"), "leaked password mention")
			}
		}
	})

	t.Run("MethodNotAllowed trigger", func(t *testing.T) {
		status, env := executeRequest(t, &taxonomyStore{}, http.MethodPost, "/monitors/1/stats", nil)
		assert.Equal(t, http.StatusMethodNotAllowed, status)
		assert.Equal(t, apierr.CodeMethodNotAllowed, env["code"])
		assert.Equal(t, "method not allowed", env["error"])
	})

	t.Run("Validation failure trigger carries FieldErrors in details", func(t *testing.T) {
		body := []byte(`{"name":"","contract_ids":[]}`)
		status, env := executeRequest(t, &taxonomyStore{}, http.MethodPost, "/monitors", body)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, apierr.CodeInvalidArgument, env["code"])
		assert.Equal(t, "validation failed", env["error"])
		details, ok := env["details"].([]any)
		require.True(t, ok)
		assert.Len(t, details, 2)
	})
}
