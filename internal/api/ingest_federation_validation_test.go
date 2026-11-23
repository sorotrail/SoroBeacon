package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// recordingIngestStore records the alerts the ingest handler stores. The
// validation cases are about what never reaches the store, so the assertion
// that matters is the call count, not just the response.
type recordingIngestStore struct {
	store.Store
	mu     sync.Mutex
	alerts []store.Alert
}

func (s *recordingIngestStore) CreateAlert(_ context.Context, a *store.Alert) (store.AlertOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := *a
	stored.Payload = append(json.RawMessage(nil), a.Payload...)
	stored.ID = int64(len(s.alerts) + 1)
	s.alerts = append(s.alerts, stored)
	a.ID = stored.ID
	return store.AlertCreated, nil
}

func (s *recordingIngestStore) created() []store.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Alert(nil), s.alerts...)
}

// ingestTokenStore is the small slice of auth.TokenStore a scoped-token test
// needs. Mint writes one row here and Authenticate reads it back by digest,
// so the scope path is exercised end to end without a database.
type ingestTokenStore struct {
	auth.TokenStore
	mu     sync.Mutex
	tokens map[string]*auth.Token
}

func newIngestTokenStore() *ingestTokenStore {
	return &ingestTokenStore{tokens: map[string]*auth.Token{}}
}

func (s *ingestTokenStore) CreateAPIToken(_ context.Context, t *auth.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *t
	s.tokens[t.Hash] = &copy
	return nil
}

func (s *ingestTokenStore) TokenByHash(_ context.Context, hash string) (*auth.Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[hash]
	if !ok {
		return nil, false, nil
	}
	copy := *t
	return &copy, true, nil
}

func (s *ingestTokenStore) ListAPITokens(context.Context) ([]auth.Token, error) {
	return nil, nil
}

func (s *ingestTokenStore) RevokeAPIToken(context.Context, int64) error { return nil }

func (s *ingestTokenStore) TouchAPIToken(context.Context, int64, time.Time) error { return nil }

// ingestHandler builds the real API router over st. a is nil for the
// unauthenticated cases; maxBody <= 0 keeps the production default.
func ingestHandler(st store.Store, a *auth.Authenticator, maxBody int64) http.Handler {
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	if a != nil {
		s.WithAuth(a)
	}
	if maxBody > 0 {
		s.WithMaxBodyBytes(maxBody)
	}
	return s.Routes()
}

// federatedAlertBody marshals a payload through notify.FederatedAlert so the
// tests use the same wire shape the federation notifier sends.
func federatedAlertBody(t *testing.T, fa notify.FederatedAlert) []byte {
	t.Helper()
	body, err := json.Marshal(fa)
	require.NoError(t, err)
	return body
}

// validFederatedAlert is the payload every validation case starts from, so a
// failing case differs from an accepted one only in the field under test.
func validFederatedAlert() notify.FederatedAlert {
	return notify.FederatedAlert{
		Alert: notify.Alert{
			MonitorID:   7,
			RuleID:      8,
			EventID:     "evt-1",
			Severity:    "critical",
			ContractID:  "C1",
			EventName:   "transfer",
			Payload:     json.RawMessage(`{"contract_id":"C1","event_name":"transfer"}`),
			MonitorName: "peer monitor",
			CreatedAt:   time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		},
		Origin:   "peer-a",
		HopCount: 3,
	}
}

func doIngest(t *testing.T, h http.Handler, body []byte, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

// Every rejected request must be rejected before the store is touched: a 400
// that still wrote a row is the failure mode this table is here to catch.
func TestIngestRejectsInvalidFederationEnvelopes(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "missing event id",
			body: func() []byte {
				fa := validFederatedAlert()
				fa.Alert.EventID = ""
				return federatedAlertBody(t, fa)
			}(),
			want: "event_id is required",
		},
		{
			name: "hop count at the limit",
			body: func() []byte {
				fa := validFederatedAlert()
				fa.HopCount = notify.MaxHopCount
				return federatedAlertBody(t, fa)
			}(),
			want: "hop limit reached",
		},
		{
			name: "empty origin",
			body: func() []byte {
				fa := validFederatedAlert()
				fa.Origin = ""
				return federatedAlertBody(t, fa)
			}(),
			want: "origin is required",
		},
		{
			name: "malformed json",
			body: []byte(`{"alert": {"event_id": "evt-1"},`),
			want: "invalid JSON",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &recordingIngestStore{}
			res := doIngest(t, ingestHandler(st, nil, 0), tt.body, "")

			assert.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
			assert.Contains(t, res.Body.String(), tt.want)
			assert.Empty(t, st.created(), "a rejected payload must not be stored")
		})
	}
}

// The body limit is enforced by middleware for a declared oversized length
// and by readJSON for a streamed body whose length is not known up front.
// Both paths must answer 413 without reaching the handler's store call.
func TestIngestRejectsOversizedBodyBeforeStoring(t *testing.T) {
	const limit = 256
	tests := []struct {
		name      string
		streaming bool
	}{
		{name: "declared content length"},
		{name: "unknown content length", streaming: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &recordingIngestStore{}
			body := bytes.Repeat([]byte("a"), limit+1)
			if tt.streaming {
				body = []byte(`{"padding":"` + string(bytes.Repeat([]byte("a"), limit+1)) + `"}`)
			}
			req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader(body))
			if tt.streaming {
				// A chunked request has no declared length; MaxBytesReader
				// still bounds the read and readJSON maps the error to 413.
				req.ContentLength = -1
			}
			res := httptest.NewRecorder()
			ingestHandler(st, nil, limit).ServeHTTP(res, req)

			assert.Equal(t, http.StatusRequestEntityTooLarge, res.Code, res.Body.String())
			assert.Contains(t, res.Body.String(), "request body too large")
			assert.Empty(t, st.created(), "an oversized payload must not be stored")
		})
	}
}

// A valid payload is stored once, with the three federation stamps plus the
// sender's ids, and acknowledged with the created alert's id.
func TestIngestStoresAValidPayloadWithFederationStamps(t *testing.T) {
	st := &recordingIngestStore{}
	res := doIngest(t, ingestHandler(st, nil, 0), federatedAlertBody(t, validFederatedAlert()), "")

	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	alerts := st.created()
	require.Len(t, alerts, 1)

	stored := alerts[0]
	assert.True(t, stored.Federated)
	assert.Equal(t, "peer-a", stored.FederationOrigin)
	assert.Equal(t, int64(7), stored.SourceMonitorID)
	assert.Equal(t, int64(8), stored.SourceRuleID)
	assert.Equal(t, "evt-1", stored.EventID)
	assert.Equal(t, store.SeverityCritical, stored.Severity)
	assert.Zero(t, stored.MonitorID, "the sender's monitor id is not a local key")
	assert.Zero(t, stored.RuleID, "the sender's rule id is not a local key")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(stored.Payload, &payload))
	assert.Equal(t, true, payload["_federated"])
	assert.Equal(t, "peer-a", payload["_federation_origin"])
	assert.Equal(t, float64(3), payload["_federation_hops"])
	assert.Equal(t, float64(7), payload["_federation_monitor_id"])
	assert.Equal(t, float64(8), payload["_federation_rule_id"])
	assert.Equal(t, "C1", payload["contract_id"], "the sender's payload is preserved")
	assert.Equal(t, "transfer", payload["event_name"])

	var response struct {
		Outcome string `json:"outcome"`
		AlertID int64  `json:"alert_id"`
		Origin  string `json:"origin"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &response))
	assert.Equal(t, string(store.AlertCreated), response.Outcome)
	assert.Equal(t, stored.ID, response.AlertID)
	assert.Equal(t, "peer-a", response.Origin)
}

// A payload that is not a JSON object cannot carry the stamps as fields, so
// the handler replaces it with an object holding the stamps. Losing a scalar
// or an array is deliberate: the stamps are what make the alert attributable,
// and a non-object cannot be merged into them.
func TestIngestReplacesANonObjectPayloadWithTheStamps(t *testing.T) {
	payloads := map[string]json.RawMessage{
		"array":  json.RawMessage(`[1,2,3]`),
		"string": json.RawMessage(`"not-an-object"`),
		"number": json.RawMessage(`42`),
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			st := &recordingIngestStore{}
			fa := validFederatedAlert()
			fa.Alert.Payload = payload

			res := doIngest(t, ingestHandler(st, nil, 0), federatedAlertBody(t, fa), "")
			require.Equal(t, http.StatusCreated, res.Code, res.Body.String())

			alerts := st.created()
			require.Len(t, alerts, 1)
			var got map[string]any
			require.NoError(t, json.Unmarshal(alerts[0].Payload, &got))
			assert.Equal(t, true, got["_federated"])
			assert.Equal(t, "peer-a", got["_federation_origin"])
			assert.Equal(t, float64(3), got["_federation_hops"])
			assert.Equal(t, float64(7), got["_federation_monitor_id"])
			assert.Equal(t, float64(8), got["_federation_rule_id"])
		})
	}
}

// With authentication enabled the endpoint is not an anonymous surface: a
// missing or wrong bearer is refused before validation or storage. A scoped
// token needs alerts:write; read access is not enough to inject an alert.
func TestIngestRequiresAuthenticationAndTheAlertsWriteScope(t *testing.T) {
	st := &recordingIngestStore{}
	tokens := newIngestTokenStore()
	mgr := auth.NewManager(tokens, discardLogger())
	a := auth.New([]string{"static-peer"}, 0).WithTokens(mgr)
	h := ingestHandler(st, a, 0)

	res := doIngest(t, h, federatedAlertBody(t, validFederatedAlert()), "")
	assert.Equal(t, http.StatusUnauthorized, res.Code)
	assert.Empty(t, st.created(), "an unauthenticated payload must not be stored")

	res = doIngest(t, h, federatedAlertBody(t, validFederatedAlert()), "wrong-token")
	assert.Equal(t, http.StatusUnauthorized, res.Code)
	assert.Empty(t, st.created(), "a wrong credential must not be stored")

	ctx := workspace.With(context.Background(), workspace.Default)
	writeToken, _, err := mgr.Mint(ctx, "peer-write", []auth.Scope{auth.ScopeAlertsWrite}, time.Time{})
	require.NoError(t, err)
	readToken, _, err := mgr.Mint(ctx, "peer-read", []auth.Scope{auth.ScopeAlertsRead}, time.Time{})
	require.NoError(t, err)

	res = doIngest(t, h, federatedAlertBody(t, validFederatedAlert()), writeToken)
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	assert.Len(t, st.created(), 1)

	res = doIngest(t, h, federatedAlertBody(t, validFederatedAlert()), readToken)
	assert.Equal(t, http.StatusForbidden, res.Code)
	assert.Len(t, st.created(), 1, "a read-only token must not inject an alert")

	scopes, ok := requiredScopes(http.MethodPost, "/ingest")
	require.True(t, ok, "ingest must be in the scope table; an absent entry denies every scoped token")
	assert.Equal(t, []auth.Scope{auth.ScopeAlertsWrite}, scopes)
}

// The handler writes a 201 for a created alert and a 422 for a payload the
// store refuses; a 400 is reserved for a malformed envelope. This pins the
// status boundary the federation sender uses to decide whether to retry.
func TestIngestStatusBoundaryForStoreOutcomes(t *testing.T) {
	st := &recordingIngestStore{}
	res := doIngest(t, ingestHandler(st, nil, 0), federatedAlertBody(t, validFederatedAlert()), "")
	assert.Equal(t, http.StatusCreated, res.Code)

	// A malformed envelope never reaches CreateAlert, so the store's outcome
	// cannot turn it into a 422.
	fa := validFederatedAlert()
	fa.Alert.EventID = ""
	res = doIngest(t, ingestHandler(st, nil, 0), federatedAlertBody(t, fa), "")
	assert.Equal(t, http.StatusBadRequest, res.Code)
}

// The error response for a malformed body must not echo the request body:
// federated payloads can contain contract data, and an invalid JSON error
// from encoding/json quotes a byte offset, not the document.
func TestIngestMalformedJSONDoesNotEchoTheBody(t *testing.T) {
	st := &recordingIngestStore{}
	body := []byte(`{"alert":{"event_id":"SECRET-EVENT"},"origin":`)
	res := doIngest(t, ingestHandler(st, nil, 0), body, "")

	assert.Equal(t, http.StatusBadRequest, res.Code)
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "SECRET-EVENT")
	assert.Empty(t, st.created())
}
