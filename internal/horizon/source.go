// Package horizon reads contract events from a Horizon server's REST API.
// Horizon retains full historical transaction metadata (result_meta_xdr)
// which contains Soroban contract events, enabling deep backfill past the
// RPC's ~7 day retention window.
package horizon

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// Source implements poller.EventSource against a Horizon server.
type Source struct {
	client ClientInterface
	decoder stellar.Decoder
}

// NewSource wires a Horizon event source.
func NewSource(c ClientInterface, decoder stellar.Decoder) *Source {
	return &Source{client: c, decoder: decoder}
}

// LatestLedger reports the Horizon server's newest ingested ledger.
func (s *Source) LatestLedger(ctx context.Context) (uint32, error) {
	return s.client.GetLatestLedger(ctx)
}

// OldestLedger returns 0 (unknown) as Horizon retention varies by deployment.
func (s *Source) OldestLedger(ctx context.Context) (uint32, error) {
	return s.client.OldestLedger(ctx)
}

// encodeCursor packs the contract index and Horizon cursor into one opaque token.
func encodeCursor(contractIdx int, horizonCursor string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(contractIdx) + "|" + horizonCursor))
}

// decodeCursor unpacks encodeCursor's output. An empty/unparseable cursor means
// "start of cycle": contract 0, no Horizon cursor.
func decodeCursor(cursor string) (contractIdx int, horizonCursor string) {
	if cursor == "" {
		return 0, ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ""
	}
	b, rest, ok := strings.Cut(string(raw), "|")
	if !ok {
		return 0, ""
	}
	n, err := strconv.Atoi(b)
	if err != nil {
		return 0, ""
	}
	return n, rest
}

// FetchEvents pages Horizon's /accounts/{contract_id}/transactions for each
// watched contract. The cursor encodes which contract we're on and the
// Horizon cursor for that contract's transaction stream.
func (s *Source) FetchEvents(ctx context.Context, startLedger uint32, watch []poller.Watch, cursor string, limit int) (poller.FetchPage, error) {
	if limit <= 0 {
		limit = stellar.DefaultEventsLimit
	}

	contractIdx, horizonCursor := decodeCursor(cursor)
	if contractIdx >= len(watch) {
		return poller.FetchPage{}, nil
	}

	w := watch[contractIdx]

	// Fetch transactions for this contract
	events, nextHorizonCursor, latestLedger, err := s.client.FetchTransactions(ctx, w.ContractID, startLedger, horizonCursor, limit)
	if err != nil {
		return poller.FetchPage{}, err
	}

	page := poller.FetchPage{LatestLedger: latestLedger}
	for _, ev := range events {
		decoded, err := DecodeEvent(ctx, s.decoder, ev)
		if err != nil {
			// A single undecodable event must not stall the cycle
			continue
		}
		page.Events = append(page.Events, decoded)
	}

	// If we have more transactions for this contract, continue with the Horizon cursor
	if nextHorizonCursor != "" {
		page.NextCursor = encodeCursor(contractIdx, nextHorizonCursor)
		return page, nil
	}

	// This contract is drained. Move to the next contract.
	if contractIdx+1 >= len(watch) {
		return page, nil
	}
	page.NextCursor = encodeCursor(contractIdx+1, "")
	return page, nil
}

// Compile-time interface checks.
var _ poller.EventSource = (*Source)(nil)
var _ poller.RetentionReporter = (*Source)(nil)
var _ interface {
	GetHealth(ctx context.Context) (*stellar.Health, error)
} = (*Client)(nil)