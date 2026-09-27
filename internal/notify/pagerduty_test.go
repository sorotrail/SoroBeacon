package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagerDutyRequest is the shape SoroBeacon must send to the Events API.
type pagerDutyRequest struct {
	RoutingKey  string `json:"routing_key"`
	EventAction string `json:"event_action"`
	DedupKey    string `json:"dedup_key"`
	Payload     struct {
		Summary       string         `json:"summary"`
		Source        string         `json:"source"`
		Severity      string         `json:"severity"`
		CustomDetails map[string]any `json:"custom_details"`
	} `json:"payload"`
}

func TestPagerDutyPayloadAndDedupKey(t *testing.T) {
	var rawBodies [][]byte
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rawBodies = append(rawBodies, raw)
		paths = append(paths, r.URL.Path)
		fmt.Fprint(w, `{"status":"success"}`)
	}))
	defer srv.Close()

	n, err := NewPagerDuty(json.RawMessage(fmt.Sprintf(
		`{"routing_key": "rk-secret", "events_url": %q}`, srv.URL)))
	require.NoError(t, err)

	payload, _ := json.Marshal(map[string]any{"event_name": "transfer", "ledger": 7})
	alert := Alert{
		ID: 1, MonitorID: 9, MonitorName: "m1", RuleID: 3, RuleType: "event_emitted",
		EventID: "ev-1", ContractID: "CABC", Ledger: 7, TxHash: "deadbeef", Payload: payload,
	}
	require.NoError(t, n.Send(context.Background(), alert))
	// Sending the same alert again must reuse the dedup key so PagerDuty
	// folds it into the incident it already opened.
	require.NoError(t, n.Send(context.Background(), alert))

	require.Len(t, rawBodies, 2)
	assert.Equal(t, "/v2/enqueue", paths[0])
	for _, raw := range rawBodies {
		var got pagerDutyRequest
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Equal(t, "rk-secret", got.RoutingKey)
		assert.Equal(t, "trigger", got.EventAction)
		assert.Equal(t, "3:ev-1", got.DedupKey, "dedup key mirrors (rule_id, event_id)")
		assert.Equal(t, "warning", got.Payload.Severity, "severity defaults to warning")
		assert.Contains(t, got.Payload.Summary, "m1")
		assert.Equal(t, "CABC", got.Payload.Source)
		assert.Equal(t, "transfer", got.Payload.CustomDetails["event_name"])
		assert.EqualValues(t, 9, got.Payload.CustomDetails["monitor_id"])
	}
}

func TestPagerDutySeverityValidation(t *testing.T) {
	base := func(sev string) string {
		if sev == "" {
			return `{"routing_key": "rk"}`
		}
		return fmt.Sprintf(`{"routing_key": "rk", "severity": %q}`, sev)
	}
	for _, sev := range []string{"critical", "error", "warning", "info"} {
		_, err := NewPagerDuty(json.RawMessage(base(sev)))
		assert.NoError(t, err, "severity %q is accepted", sev)
	}
	_, err := NewPagerDuty(json.RawMessage(base("page")))
	assert.Error(t, err, "an unknown severity must be rejected at construction")
	_, err = NewPagerDuty(json.RawMessage(`{"severity": "warning"}`))
	assert.Error(t, err, "routing_key is required")
}

func TestPagerDutyNon2xxIsErrorWithoutRoutingKey(t *testing.T) {
	const key = "rk-do-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	n, err := NewPagerDuty(json.RawMessage(fmt.Sprintf(`{"routing_key": %q, "events_url": %q}`, key, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, RuleID: 2, EventID: "e"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.NotContains(t, err.Error(), key, "errors must never leak the routing key")
}
