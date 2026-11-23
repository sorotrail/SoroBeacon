package notify

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An event-driven alert names the event, ledger and transaction that matched.
func TestRenderTextEventAlert(t *testing.T) {
	got, err := RenderText(Alert{
		ID:          1,
		MonitorName: "m1",
		RuleID:      2,
		RuleType:    "event_emitted",
		EventID:     "0000000012884905986-0000000000",
		ContractID:  "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		EventName:   "transfer",
		Ledger:      5990,
		TxHash:      "abc123",
		CreatedAt:   time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Contains(t, got, "Event: transfer")
	assert.Contains(t, got, "Ledger: 5990")
	assert.Contains(t, got, "Tx: abc123")
	assert.Contains(t, got, "Contract: CA7QYNF7")
}

// An absence alert has no event, ledger or transaction to report; naming them
// as empty fields would read like a broken alert rather than a contract gone
// quiet, so the template reports the silence instead.
func TestRenderTextAbsenceAlert(t *testing.T) {
	got, err := RenderText(Alert{
		ID:          2,
		MonitorName: "m1",
		RuleID:      3,
		RuleType:    "absence_of_event",
		EventID:     "absence-123",
		EventName:   "heartbeat",
		Silence:     31 * time.Minute,
		CreatedAt:   time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Contains(t, got, "No heartbeat for 31m0s")
	assert.NotContains(t, got, "Event: heartbeat", "the awaited event is named as missing, not as matched")
	assert.NotContains(t, got, "Ledger:")
	assert.NotContains(t, got, "Tx:")
	assert.NotContains(t, got, "Contract:")
}

// RenderText is total: a caller with a half-populated alert gets a message,
// not a template error that would fail the delivery.
func TestRenderTextSparseAlert(t *testing.T) {
	got, err := RenderText(Alert{MonitorName: "m1", RuleType: "event_emitted"})
	require.NoError(t, err)
	assert.Contains(t, got, "SoroBeacon alert: m1")
}
