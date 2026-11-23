package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// ingestStore is a store whose CreateAlert fails with a chosen error, so the
// handler's error mapping can be exercised without a database.
type ingestStore struct {
	store.Store
	err error
}

func (s *ingestStore) CreateAlert(context.Context, *store.Alert) (store.AlertOutcome, error) {
	if s.err != nil {
		return "", s.err
	}
	return store.AlertCreated, nil
}

// errForeignKey is what a store reports when a row names a record that does
// not exist. The constraint's name is schema metadata, not a secret, and it
// must not reach the response.
func errForeignKey() error {
	return &store.ConstraintError{Kind: store.ConstraintForeignKey, Constraint: "alerts_monitor_id_fkey"}
}

// errCheck is what a store reports when a value fails a CHECK constraint.
func errCheck() error {
	return &store.ConstraintError{Kind: store.ConstraintCheck, Constraint: "alerts_severity_check"}
}

// ingestBody is a well-formed federated alert naming a monitor and a rule
// the receiving instance does not have.
func ingestBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"alert": map[string]any{
			"monitor_id": 4242,
			"rule_id":    99,
			"event_id":   "evt-1",
			"payload":    map[string]any{"contract_id": "C1"},
		},
		"origin":    "peer-a",
		"hop_count": 1,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return body
}

func postIngest(t *testing.T, st store.Store) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader(ingestBody(t)))
	res := httptest.NewRecorder()
	authServerOn(st, nil).ServeHTTP(res, req)
	return res.Result()
}

// A payload that names a monitor or a rule this instance does not have is a
// client error, not a server fault: the sender can never make it succeed by
// sending it again. Answering 500 would make the failure look transient on
// the sending side, which retries three times and then dead-letters.
func TestIngestMapsPayloadFailuresToUnprocessable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "a foreign-key violation is the payload's fault",
			err:  errForeignKey(),
		},
		{
			name: "a check violation is the payload's fault",
			err:  errCheck(),
		},
		{
			name: "a missing monitor or rule is the payload's fault",
			err:  store.ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := postIngest(t, &ingestStore{err: tt.err})
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", res.StatusCode)
			}
			body, _ := io.ReadAll(res.Body)
			text := string(body)
			if strings.Contains(strings.ToLower(text), "select ") ||
				strings.Contains(strings.ToLower(text), "insert ") {
				t.Fatalf("response echoes SQL: %s", text)
			}
			if !strings.Contains(text, "error") {
				t.Fatalf("response has no error message: %s", text)
			}
		})
	}
}

// A store failure that is not about the payload is still a server fault, and
// it must not be dressed up as a client error: the sender is right to retry.
func TestIngestKeepsUnexpectedStoreFailuresAsInternal(t *testing.T) {
	res := postIngest(t, &ingestStore{err: errors.New("connection reset by peer")})
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
}

// A payload the store accepts is stored and acknowledged, whatever the
// payload's ids are: the handler's job is to record what it was sent.
func TestIngestAcceptsAPayloadTheStoreTakes(t *testing.T) {
	res := postIngest(t, &ingestStore{})
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "peer-a") {
		t.Fatalf("response does not name the origin: %s", body)
	}
}
