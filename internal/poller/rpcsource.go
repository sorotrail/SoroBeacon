package poller

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// RPCSource reads events from a Stellar RPC node's getEvents and decodes
// them locally. It is the default EventSource.
//
// The RPC caps requests at 5 filters with 5 contract IDs each, so the watch
// list is split into batches; the batch position is encoded in the opaque
// cursor ("batch|rpc-cursor"), keeping the source stateless across calls.
type RPCSource struct {
	rpc     stellar.Client
	decoder stellar.Decoder
}

// NewRPCSource wires an RPCSource from a client and decoder.
func NewRPCSource(rpc stellar.Client, decoder stellar.Decoder) *RPCSource {
	return &RPCSource{rpc: rpc, decoder: decoder}
}

func (s *RPCSource) LatestLedger(ctx context.Context) (uint32, error) {
	latest, err := s.rpc.GetLatestLedger(ctx)
	if err != nil {
		return 0, err
	}
	return latest.Sequence, nil
}

// encodeCursor packs batch index and the RPC's own cursor into one opaque
// token. Base64 keeps it URL- and header-safe for any future transport.
func encodeCursor(batch int, rpcCursor string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(batch) + "|" + rpcCursor))
}

// decodeCursor unpacks encodeCursor's output. An empty/unparseable cursor
// means "start of cycle": batch 0, no RPC cursor.
func decodeCursor(cursor string) (batch int, rpcCursor string) {
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

func (s *RPCSource) FetchEvents(ctx context.Context, startLedger uint32, contracts []string, cursor string, limit int) (FetchPage, error) {
	if limit <= 0 {
		limit = stellar.DefaultEventsLimit
	}

	// getEvents caps requests at MaxFiltersPerRequest filters with
	// MaxContractIDsPerFilter contract IDs each. Group the watch list into
	// request-sized batches; the cursor's batch index addresses a group.
	groups := groupFilters(buildFilters(contracts))

	batchIdx, rpcCursor := decodeCursor(cursor)
	if batchIdx >= len(groups) {
		return FetchPage{}, nil
	}
	batch := groups[batchIdx]

	// The RPC forbids startLedger alongside a cursor: the cursor already
	// encodes the position.
	req := stellar.GetEventsRequest{
		StartLedger: startLedger,
		Filters:     batch,
		Pagination:  &stellar.Pagination{Cursor: rpcCursor, Limit: limit},
	}
	if rpcCursor != "" {
		req.StartLedger = 0
	}
	res, err := s.rpc.GetEvents(ctx, req)
	if err != nil {
		return FetchPage{}, err
	}

	page := FetchPage{LatestLedger: res.LatestLedger}
	for i := range res.Events {
		decoded, err := s.decoder.DecodeEvent(res.Events[i])
		if err != nil {
			// A single undecodable event must not stall the cycle; the
			// source drops it and the poller keeps going. Decoding errors
			// are logged by the caller's pipeline, not here.
			continue
		}
		page.Events = append(page.Events, decoded)
	}

	fullPage := len(res.Events) >= limit
	if !fullPage {
		// This batch is drained. Move to the next; when the last batch is
		// done, the cycle is done.
		if batchIdx+1 >= len(groups) {
			return page, nil
		}
		page.NextCursor = encodeCursor(batchIdx+1, "")
		return page, nil
	}

	next := res.Cursor
	if next == "" {
		// Older RPC versions: page with the last event's token/id.
		last := res.Events[len(res.Events)-1]
		next = last.PagingToken
		if next == "" {
			next = last.ID
		}
	}
	if next == "" {
		// A full page with no continuation token: treat as drained to
		// avoid an infinite loop re-reading the same page.
		if batchIdx+1 >= len(groups) {
			return page, nil
		}
		page.NextCursor = encodeCursor(batchIdx+1, "")
		return page, nil
	}
	page.NextCursor = encodeCursor(batchIdx, next)
	return page, nil
}

// groupFilters packs filters into request-sized groups: each group holds
// at most MaxFiltersPerRequest filters, which is the RPC's per-request cap.
func groupFilters(filters []stellar.EventFilter) [][]stellar.EventFilter {
	var groups [][]stellar.EventFilter
	for i := 0; i < len(filters); i += stellar.MaxFiltersPerRequest {
		end := min(i+stellar.MaxFiltersPerRequest, len(filters))
		groups = append(groups, filters[i:end])
	}
	return groups
}

var _ EventSource = (*RPCSource)(nil)
