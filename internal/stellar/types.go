// Package stellar talks to a Stellar RPC node (JSON-RPC 2.0 over HTTP) and
// decodes Soroban contract events into plain Go values.
package stellar

import (
	"encoding/json"
	"time"
)

// Limits imposed by the getEvents RPC method. The poller batches monitored
// contracts across filters and requests so it never exceeds them.
const (
	// MaxFiltersPerRequest is the hard cap on filters in one getEvents call.
	MaxFiltersPerRequest = 5
	// MaxContractIDsPerFilter is the cap on contractIds within one filter.
	MaxContractIDsPerFilter = 5
	// DefaultEventsLimit is the page size requested from getEvents.
	DefaultEventsLimit = 100
)

// EventFilter narrows getEvents results. Within a filter, contractIds are
// OR-ed together; a filter with topics matches contractIds AND topics.
type EventFilter struct {
	Type        string     `json:"type,omitempty"` // "contract", "system", "diagnostic"
	ContractIDs []string   `json:"contractIds,omitempty"`
	Topics      [][]string `json:"topics,omitempty"`
}

// Pagination controls getEvents paging. When Cursor is set, StartLedger must
// be omitted from the request.
type Pagination struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// GetEventsRequest are the params for the getEvents RPC method.
type GetEventsRequest struct {
	StartLedger uint32        `json:"startLedger,omitempty"` // inclusive
	EndLedger   uint32        `json:"endLedger,omitempty"`   // exclusive
	Filters     []EventFilter `json:"filters,omitempty"`
	Pagination  *Pagination   `json:"pagination,omitempty"`
	// XDRFormat is "json" or "base64". The HTTP client sets this itself and
	// falls back to base64 against RPC versions that reject "json".
	XDRFormat string `json:"xdrFormat,omitempty"`
}

// Event is one contract event as returned by getEvents. Depending on the
// xdrFormat the RPC supports, topics/value arrive either as base64 XDR
// (Topic/Value) or as ready-to-read JSON (TopicJSON/ValueJSON).
type Event struct {
	ID                       string    `json:"id"` // TOID-based, unique per event
	Type                     string    `json:"type"`
	Ledger                   uint32    `json:"ledger"`
	LedgerClosedAt           time.Time `json:"ledgerClosedAt"`
	ContractID               string    `json:"contractId"`
	PagingToken              string    `json:"pagingToken"`
	InSuccessfulContractCall bool      `json:"inSuccessfulContractCall"`
	TxHash                   string    `json:"txHash"`

	Topic []string `json:"topic,omitempty"` // base64 XDR ScVals
	Value string   `json:"value,omitempty"` // base64 XDR ScVal

	TopicJSON []json.RawMessage `json:"topicJson,omitempty"`
	ValueJSON json.RawMessage   `json:"valueJson,omitempty"`
}

// GetEventsResult is the getEvents response. Newer RPC versions return a
// top-level cursor for the next page; when absent, the last event's
// PagingToken serves the same purpose. A short page (< limit) means the
// stream is drained for now.
type GetEventsResult struct {
	Events       []Event `json:"events"`
	LatestLedger uint32  `json:"latestLedger"`
	OldestLedger uint32  `json:"oldestLedger,omitempty"`
	Cursor       string  `json:"cursor,omitempty"`
}

// LatestLedger is the getLatestLedger response.
type LatestLedger struct {
	ID              string `json:"id"`
	Sequence        uint32 `json:"sequence"`
	ProtocolVersion int    `json:"protocolVersion"`
}

// Health is the getHealth response.
type Health struct {
	Status                string `json:"status"`
	LatestLedger          uint32 `json:"latestLedger"`
	OldestLedger          uint32 `json:"oldestLedger"`
	LedgerRetentionWindow uint32 `json:"ledgerRetentionWindow"`
}

// DecodedEvent is an Event with its topics and value decoded into plain Go
// values (see Decoder for the value vocabulary). This is what rule
// evaluators operate on.
type DecodedEvent struct {
	ID             string
	ContractID     string
	Ledger         uint32
	LedgerClosedAt time.Time
	TxHash         string
	// Topics are the decoded topic ScVals. By Soroban convention the first
	// topic is the event name as a symbol (string).
	Topics []any
	// Value is the decoded event data ScVal.
	Value any
}

// EventName returns the first topic if it is a string (the conventional
// Soroban event name), or "" otherwise.
func (e *DecodedEvent) EventName() string {
	if len(e.Topics) > 0 {
		if s, ok := e.Topics[0].(string); ok {
			return s
		}
	}
	return ""
}
