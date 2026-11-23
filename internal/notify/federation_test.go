package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFederationWithOrigin_Valid(t *testing.T) {
	cfg := `{"url":"https://upstream.example.com","token":"secret-token"}`
	n, err := NewFederationWithOrigin(json.RawMessage(cfg), "instance-a")
	require.NoError(t, err)
	assert.NotNil(t, n)
}

func TestNewFederationWithOrigin_MissingURL(t *testing.T) {
	cfg := `{"token":"secret-token"}`
	_, err := NewFederationWithOrigin(json.RawMessage(cfg), "instance-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url is required")
}

func TestNewFederationWithOrigin_MissingToken(t *testing.T) {
	cfg := `{"url":"https://upstream.example.com"}`
	_, err := NewFederationWithOrigin(json.RawMessage(cfg), "instance-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token is required")
}

func TestNewFederationWithOrigin_MissingOrigin(t *testing.T) {
	cfg := `{"url":"https://upstream.example.com","token":"secret-token"}`
	_, err := NewFederationWithOrigin(json.RawMessage(cfg), "  ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INSTANCE_NAME")
}

func TestFactoryFederationRequiresOrigin(t *testing.T) {
	cfg := json.RawMessage(`{"url":"https://upstream.example.com","token":"secret-token"}`)

	_, err := DefaultFactory().New(TypeFederation, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INSTANCE_NAME")

	n, err := DefaultFactory().WithFederationOrigin(" instance-a ").New(TypeFederation, cfg)
	require.NoError(t, err)
	assert.Equal(t, "instance-a", n.(*Federation).origin)
}

func TestFederation_Send(t *testing.T) {
	var received FederatedAlert
	var authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := json.RawMessage(`{"url":"` + srv.URL + `","token":"test-token"}`)
	n, err := DefaultFactory().WithFederationOrigin("instance-a").New(TypeFederation, cfg)
	require.NoError(t, err)

	alert := Alert{
		ID:          42,
		MonitorID:   1,
		MonitorName: "test-monitor",
		RuleID:      2,
		RuleType:    "event_emitted",
		EventID:     "evt-123",
		ContractID:  "C123",
		Ledger:      100,
		CreatedAt:   time.Now().UTC(),
	}

	err = n.Send(context.Background(), alert)
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-token", authHeader)
	assert.Equal(t, int64(42), received.Alert.ID)
	assert.Equal(t, "instance-a", received.Origin)
	assert.Equal(t, 1, received.HopCount)
}
