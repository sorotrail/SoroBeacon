package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// constraintStore fails a store read with a chosen error, so the shared error
// mapping can be exercised from a handler that is not /ingest.
type constraintStore struct {
	store.Store
	err error
}

func (s *constraintStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	return nil, s.err
}

// Every handler that hands a store error to the shared mapping gets the same
// answer from it. A constraint violation is the request's fault, so it is a
// 422 rather than a 500: a 500 reads as transient on the sending side, which
// retries a request that cannot succeed.
func TestConstraintViolationIsUnprocessableEverywhere(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "foreign key",
			err:  &store.ConstraintError{Kind: store.ConstraintForeignKey, Constraint: "monitors_workspace_id_fkey"},
		},
		{
			name: "check",
			err:  &store.ConstraintError{Kind: store.ConstraintCheck, Constraint: "monitors_priority_check"},
		},
		{
			name: "unique",
			err:  &store.ConstraintError{Kind: store.ConstraintUnique, Constraint: "monitors_workspace_name_uidx"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := get(t, authServerOn(&constraintStore{err: tt.err}, nil), "/monitors/1", nil)
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", res.StatusCode)
			}
			body, _ := io.ReadAll(res.Body)
			text := string(body)
			if strings.Contains(strings.ToLower(text), "select ") {
				t.Fatalf("response echoes SQL: %s", text)
			}
			if strings.Contains(text, "monitors_workspace_name_uidx") {
				t.Fatalf("response names the constraint: %s", text)
			}
		})
	}
}

// A missing row is still a 404, and an unexpected store failure is still a
// 500: the constraint mapping must not swallow either of them.
func TestOtherStoreErrorsKeepTheirStatus(t *testing.T) {
	t.Run("not found is a 404", func(t *testing.T) {
		res := get(t, authServerOn(&constraintStore{err: store.ErrNotFound}, nil), "/monitors/1", nil)
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", res.StatusCode)
		}
	})

	t.Run("anything else is a 500", func(t *testing.T) {
		res := get(t, authServerOn(&constraintStore{err: errors.New("connection refused by the database")}, nil), "/monitors/1", nil)
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", res.StatusCode)
		}
	})
}
