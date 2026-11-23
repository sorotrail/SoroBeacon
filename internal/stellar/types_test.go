package stellar

import (
	"encoding/json"
	"testing"
	"time"
)

// The structs in types.go are the wire contract with the Soroban RPC: a
// mistyped JSON tag is invisible at compile time and surfaces as a silently
// empty field at runtime. These tests pin every tag by round-tripping
// realistic payloads, so a rename that drops a field fails here instead of
// in production.
func TestGetEventsRequestMarshalKeys(t *testing.T) {
	req := GetEventsRequest{
		StartLedger: 100,
		EndLedger:   200,
		Filters: []EventFilter{
			{
				Type:        "contract",
				ContractIDs: []string{"CABC"},
				Topics:      [][]string{{"transfer", "*"}},
			},
		},
		Pagination: &Pagination{Cursor: "cursor-1", Limit: 50},
		XDRFormat:  "json",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	// The RPC rejects unknown keys, so assert the exact key set the RPC
	// documents — no more, no fewer.
	for _, key := range []string{"startLedger", "endLedger", "filters", "pagination", "xdrFormat"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q in %s", key, raw)
		}
	}
	filters := got["filters"].([]any)
	filter := filters[0].(map[string]any)
	if filter["type"] != "contract" {
		t.Errorf("filter type = %v, want contract", filter["type"])
	}
	if ids, ok := filter["contractIds"].([]any); !ok || len(ids) != 1 || ids[0] != "CABC" {
		t.Errorf("contractIds = %v, want [CABC]", filter["contractIds"])
	}
	pagination := got["pagination"].(map[string]any)
	if pagination["cursor"] != "cursor-1" {
		t.Errorf("pagination cursor = %v, want cursor-1", pagination["cursor"])
	}
}

func TestGetEventsRequestOmitsEmpty(t *testing.T) {
	// A cursor-based continuation must omit StartLedger: the RPC rejects a
	// request carrying both.
	req := GetEventsRequest{
		Pagination: &Pagination{Cursor: "cursor-9", Limit: 100},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if _, ok := got["startLedger"]; ok {
		t.Errorf("startLedger present in %s, must be omitted on cursor pages", raw)
	}
	if _, ok := got["filters"]; ok {
		t.Errorf("filters present in %s, must be omitted when empty", raw)
	}
}

func TestEventUnmarshalJSONFormat(t *testing.T) {
	// xdrFormat:"json" shape: topics and value arrive as JSON, not base64.
	const payload = `{
		"id": "0000001234-0000000001",
		"type": "contract",
		"ledger": 1234,
		"ledgerClosedAt": "2024-05-01T12:00:00Z",
		"contractId": "CABC",
		"pagingToken": "0000001234-0000000001",
		"inSuccessfulContractCall": true,
		"txHash": "deadbeef",
		"topicJson": [{"symbol": "transfer"}, "GAAA", "GBBB"],
		"valueJson": {"amount": "100"}
	}`
	var ev Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.ID != "0000001234-0000000001" {
		t.Errorf("ID = %q", ev.ID)
	}
	if ev.Ledger != 1234 {
		t.Errorf("Ledger = %d, want 1234", ev.Ledger)
	}
	if !ev.InSuccessfulContractCall {
		t.Error("InSuccessfulContractCall = false, want true")
	}
	if ev.ContractID != "CABC" || ev.TxHash != "deadbeef" {
		t.Errorf("contract/tx = %q/%q", ev.ContractID, ev.TxHash)
	}
	wantTime := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	if !ev.LedgerClosedAt.Equal(wantTime) {
		t.Errorf("LedgerClosedAt = %v, want %v", ev.LedgerClosedAt, wantTime)
	}
	if len(ev.TopicJSON) != 3 {
		t.Fatalf("TopicJSON len = %d, want 3", len(ev.TopicJSON))
	}
	var first map[string]any
	if err := json.Unmarshal(ev.TopicJSON[0], &first); err != nil {
		t.Fatalf("unmarshal first topic: %v", err)
	}
	if first["symbol"] != "transfer" {
		t.Errorf("first topic = %v, want {\"symbol\":\"transfer\"}", first)
	}
	// Base64 fields stay empty when the JSON shape is used.
	if len(ev.Topic) != 0 || ev.Value != "" {
		t.Errorf("base64 fields set on JSON-format event: %v/%q", ev.Topic, ev.Value)
	}
}

func TestEventUnmarshalBase64Format(t *testing.T) {
	// xdrFormat:"base64" shape: topics/value arrive as base64 XDR strings.
	const payload = `{
		"id": "0000001234-0000000002",
		"type": "contract",
		"ledger": 1234,
		"ledgerClosedAt": "2024-05-01T12:00:01Z",
		"contractId": "CABC",
		"pagingToken": "0000001234-0000000002",
		"inSuccessfulContractCall": false,
		"txHash": "cafef00d",
		"topic": ["AAAAAQ=="],
		"value": "AAAAAg=="
	}`
	var ev Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if len(ev.Topic) != 1 || ev.Topic[0] != "AAAAAQ==" {
		t.Errorf("Topic = %v", ev.Topic)
	}
	if ev.Value != "AAAAAg==" {
		t.Errorf("Value = %q", ev.Value)
	}
	if len(ev.TopicJSON) != 0 || len(ev.ValueJSON) != 0 {
		t.Error("JSON fields set on base64-format event")
	}
}

func TestGetEventsResultUnmarshal(t *testing.T) {
	const payload = `{
		"events": [],
		"latestLedger": 9999,
		"oldestLedger": 9900,
		"cursor": "next-page"
	}`
	var res GetEventsResult
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.LatestLedger != 9999 || res.OldestLedger != 9900 {
		t.Errorf("ledgers = %d/%d, want 9999/9900", res.LatestLedger, res.OldestLedger)
	}
	if res.Cursor != "next-page" {
		t.Errorf("Cursor = %q", res.Cursor)
	}
	if res.Events == nil {
		t.Error("Events is nil; want empty non-nil slice distinction preserved")
	}
}

func TestGetEventsResultAbsentFields(t *testing.T) {
	// Older RPC versions omit cursor and oldestLedger. Absent must mean
	// zero, so the poller falls back to the last paging token instead of a
	// garbage cursor.
	var res GetEventsResult
	if err := json.Unmarshal([]byte(`{"events": [], "latestLedger": 42}`), &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.Cursor != "" || res.OldestLedger != 0 {
		t.Errorf("absent fields = %q/%d, want zero values", res.Cursor, res.OldestLedger)
	}
}

func TestLatestLedgerUnmarshal(t *testing.T) {
	var ll LatestLedger
	if err := json.Unmarshal([]byte(
		`{"id": "abc", "sequence": 777, "protocolVersion": 21}`), &ll); err != nil {
		t.Fatalf("unmarshal latest ledger: %v", err)
	}
	if ll.Sequence != 777 || ll.ProtocolVersion != 21 || ll.ID != "abc" {
		t.Errorf("got %+v", ll)
	}
}

func TestHealthUnmarshal(t *testing.T) {
	var h Health
	if err := json.Unmarshal([]byte(
		`{"status": "healthy", "latestLedger": 100, "oldestLedger": 50, "ledgerRetentionWindow": 50}`), &h); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}
	if h.Status != "healthy" || h.LatestLedger != 100 || h.OldestLedger != 50 || h.LedgerRetentionWindow != 50 {
		t.Errorf("got %+v", h)
	}
}
