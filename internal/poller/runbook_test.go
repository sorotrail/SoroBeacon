package poller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPollerAttachesInheritedRunbookAndGuidanceToAlerts(t *testing.T) {
	t.Parallel()

	rpc := &fakeRPC{
		latest: 10,
		responses: []*stellar.GetEventsResult{{
			LatestLedger: 10,
			Events:       []stellar.Event{transferEvent("0000000042949672960-0000000001", 10, "500")},
		}},
	}

	st := newFakeStore()
	st.state = store.IngestState{LastLedger: 9}
	st.monitors = []store.Monitor{{
		ID:               1,
		Name:             "Bridge Watch",
		ContractIDs:      []string{contractA},
		Enabled:          true,
		RunbookURL:       "https://runbooks.example.com/bridge-monitor",
		ResponseGuidance: "Monitor fallback guidance.",
	}}
	st.rules[1] = []store.Rule{{
		ID:               10,
		MonitorID:        1,
		Type:             rules.TypeEventEmitted,
		Params:           json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:          true,
		ResponseGuidance: "Rule override: verify bridge nonce.",
	}}

	disp := &fakeDispatcher{}
	p := newTestPoller(rpc, st, disp)

	require.NoError(t, p.Poll(context.Background()))

	require.Len(t, st.alerts, 1)
	assert.Equal(t, "https://runbooks.example.com/bridge-monitor", st.alerts[0].RunbookURL)
	assert.Equal(t, "Rule override: verify bridge nonce.", st.alerts[0].ResponseGuidance)

	var payloadMap map[string]any
	require.NoError(t, json.Unmarshal(st.alerts[0].Payload, &payloadMap))
	assert.Equal(t, "https://runbooks.example.com/bridge-monitor", payloadMap["runbook_url"])
	assert.Equal(t, "Rule override: verify bridge nonce.", payloadMap["response_guidance"])

	require.Len(t, disp.dispatched, 1)
	assert.Equal(t, "https://runbooks.example.com/bridge-monitor", disp.dispatched[0].RunbookURL)
	assert.Equal(t, "Rule override: verify bridge nonce.", disp.dispatched[0].ResponseGuidance)
}
