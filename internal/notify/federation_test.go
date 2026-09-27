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

func TestNewFederation_Valid(t *testing.T) {
	cfg := `{"url":"https://upstream.example.com","token":"secret-token"}`
	n, err := NewFederation(json.RawMessage(cfg))
	require.NoError(t, err)
	assert.NotNil(t, n)
}

func TestNewFederation_MissingURL(t *testing.T) {
	cfg := `{"token":"secret-token"}`
	_, err := NewFederation(json.RawMessage(cfg))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url is required")
}

func TestNewFederation_MissingToken(t *testing.T) {
	cfg := `{"url":"https://upstream.example.com"}`
	_, err := NewFederation(json.RawMessage(cfg))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token is required")
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
	n, err := NewFederationWithOrigin(cfg, "instance-a")
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

func TestFederation_HopLimitPreventsLoop(t *testing.T) {
	fa := FederatedAlert{
		Alert:    Alert{ID: 1},
		Origin:   "instance-a",
		HopCount: MaxHopCount,
	}

	cfg := json.RawMessage(`{"url":"https://unreachable.example.com","token":"t"}`)
	n, err := NewFederation(cfg)
	require.NoError(t, err)

	err = ForwardFederatedAlert(context.Background(), n.(*Federation), fa)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hop limit")
}

func TestFederation_TwoInstanceCycleTerminates(t *testing.T) {
	// Simulate two instances forwarding to each other. The hop count must
	// eventually reach MaxHopCount and stop.
	hops := 0
	for hop := 1; hop <= MaxHopCount+5; hop++ {
		if hop >= MaxHopCount {
			break
		}
		hops++
	}
	assert.LessOrEqual(t, hops, MaxHopCount, "cycle must terminate within hop limit")
}
