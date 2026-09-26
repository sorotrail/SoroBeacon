package poller

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
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

// OldestLedger reports the oldest ledger the RPC still retains events for, so
// a backfill can clamp a range that predates it. getHealth carries the value
// directly; a node that does not report it yields 0, which the backfill
// treats as "unknown".
func (s *RPCSource) OldestLedger(ctx context.Context) (uint32, error) {
	health, err := s.rpc.GetHealth(ctx)
	if err != nil {
		return 0, err
	}
	return health.OldestLedger, nil
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

func (s *RPCSource) FetchEvents(ctx context.Context, startLedger uint32, watch []Watch, cursor string, limit int) (FetchPage, error) {
	if limit <= 0 {
		limit = stellar.DefaultEventsLimit
	}

	// getEvents caps requests at MaxFiltersPerRequest filters with
	// MaxContractIDsPerFilter contract IDs each. Group the watch list into
	// request-sized batches; the cursor's batch index addresses a group.
	groups := groupFilters(buildFilters(watch))

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
		decoded, err := s.decoder.DecodeEvent(ctx, res.Events[i])
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

// buildFilters packs watched contracts into getEvents filters. Contracts with
// identical topic filters share a filter (up to MaxContractIDsPerFilter each),
// because a filter's Topics apply to every contract ID in it; unfiltered
// contracts share one group with no topics so their events are never dropped.
// Order is preserved so a watch list produces a stable request shape.
func buildFilters(watch []Watch) []stellar.EventFilter {
	type group struct {
		topics [][]string
		ids    []string
	}
	index := map[string]int{}
	var groups []group
	for _, w := range watch {
		key := topicKey(w.Topics)
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, group{topics: w.Topics})
		}
		groups[i].ids = append(groups[i].ids, w.ContractID)
	}

	var out []stellar.EventFilter
	for _, g := range groups {
		for i := 0; i < len(g.ids); i += stellar.MaxContractIDsPerFilter {
			end := min(i+stellar.MaxContractIDsPerFilter, len(g.ids))
			out = append(out, stellar.EventFilter{
				Type:        "contract",
				ContractIDs: g.ids[i:end],
				Topics:      g.topics,
			})
		}
	}
	return out
}

// topicKey renders a topic filter list as a map key. Two contracts group
// together exactly when their filters represent the same query.
func topicKey(topics [][]string) string {
	if len(topics) == 0 {
		return ""
	}
	var b strings.Builder
	for _, segment := range topics {
		b.WriteString(strings.Join(segment, "\x00"))
		b.WriteByte('\x01')
	}
	return b.String()
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

// ledgerGetter is the getLedgers capability of the RPC client. It is a narrow
// interface rather than a method on stellar.Client so sources and test
// doubles that do not care about reorg detection need not implement it.
type ledgerGetter interface {
	GetLedgers(ctx context.Context, req stellar.GetLedgersRequest) (*stellar.GetLedgersResult, error)
}

// LedgerHashes pages getLedgers across the inclusive range [from, to]. A
// client that cannot answer getLedgers (an older node, or a test double)
// reports ErrLedgerHashesUnsupported, and the poller skips reorg detection for
// that source instead of failing the cycle.
func (s *RPCSource) LedgerHashes(ctx context.Context, from, to uint32) ([]store.LedgerHash, error) {
	if from > to {
		return nil, nil
	}
	getter, ok := s.rpc.(ledgerGetter)
	if !ok {
		return nil, ErrLedgerHashesUnsupported
	}

	// One page is enough for the common case (the tracking window is far
	// below the page cap); the loop covers a larger configured window.
	limit := to - from + 1
	if limit > stellar.DefaultLedgersLimit {
		limit = stellar.DefaultLedgersLimit
	}
	var out []store.LedgerHash
	cursor := ""
	for {
		req := stellar.GetLedgersRequest{
			StartLedger: from,
			Pagination:  &stellar.Pagination{Cursor: cursor, Limit: int(limit)},
		}
		if cursor != "" {
			req.StartLedger = 0
		}
		res, err := getter.GetLedgers(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, l := range res.Ledgers {
			if l.Sequence < from {
				continue
			}
			if l.Sequence > to {
				return out, nil
			}
			out = append(out, store.LedgerHash{Ledger: l.Sequence, Hash: l.Hash})
		}
		if res.Cursor == "" || len(res.Ledgers) == 0 {
			break
		}
		cursor = res.Cursor
	}
	return out, nil
}

var _ EventSource = (*RPCSource)(nil)
var _ RetentionReporter = (*RPCSource)(nil)
var _ LedgerHashSource = (*RPCSource)(nil)
