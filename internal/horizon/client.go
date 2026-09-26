// Package horizon reads contract events from a Horizon server's REST API.
// Horizon retains full historical transaction metadata (result_meta_xdr)
// which contains Soroban contract events, enabling deep backfill past the
// RPC's ~7 day retention window.
package horizon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// ClientInterface defines the methods the Source needs from a Horizon client.
type ClientInterface interface {
	FetchTransactions(ctx context.Context, contractID string, fromLedger uint32, cursor string, limit int) ([]ContractEventFromMeta, string, uint32, error)
	GetLatestLedger(ctx context.Context) (uint32, error)
	GetHealth(ctx context.Context) (*stellar.Health, error)
	OldestLedger(ctx context.Context) (uint32, error)
}

// Client is a minimal Horizon API client for fetching contract events.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient returns a client for a Horizon base URL, e.g.
// https://horizon-testnet.stellar.org. The base URL carries no path.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

// Compile-time check that Client implements ClientInterface.
var _ ClientInterface = (*Client)(nil)

// transactionResponse is one transaction from Horizon's /accounts/{id}/transactions.
type transactionResponse struct {
	ID                string `json:"id"`
	PagingToken       string `json:"paging_token"`
	Hash              string `json:"hash"`
	Ledger            uint32 `json:"ledger"`
	CreatedAt         string `json:"created_at"`
	SourceAccount     string `json:"source_account"`
	ResultMetaXDR     string `json:"result_meta_xdr"`
	Successful        bool   `json:"successful"`
	OperationCount    int    `json:"operation_count"`
}

// transactionsResponse is the paginated response from Horizon.
type transactionsResponse struct {
	Embedded struct {
		Records []transactionResponse `json:"records"`
	} `json:"_embedded"`
	Links struct {
		Next struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
}

// ContractEventFromMeta wraps a decoded ContractEvent with its context.
type ContractEventFromMeta struct {
	Event         *xdr.ContractEvent
	ContractID    string
	Ledger        uint32
	LedgerClosedAt time.Time
	TxHash        string
	TxIndex       uint32
	OpIndex       uint32
	EventIndex    uint32
}

// FetchTransactions fetches one page of transactions for a contract.
// cursor continues a previous page; fromLedger applies only on the first page.
func (c *Client) FetchTransactions(ctx context.Context, contractID string, fromLedger uint32, cursor string, limit int) ([]ContractEventFromMeta, string, uint32, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	} else if fromLedger > 0 {
		q.Set("from_ledger", strconv.FormatUint(uint64(fromLedger), 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	q.Set("order", "asc")

	u := c.baseURL + "/accounts/" + contractID + "/transactions"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("Accept", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", 0, fmt.Errorf("horizon fetch transactions: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, "", 0, fmt.Errorf("horizon read response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", 0, fmt.Errorf("horizon status %d: %.200s", res.StatusCode, string(body))
	}

	var tr transactionsResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, "", 0, fmt.Errorf("horizon decode response: %w", err)
	}

	var events []ContractEventFromMeta
	var latestLedger uint32
	for _, tx := range tr.Embedded.Records {
		if !tx.Successful || tx.ResultMetaXDR == "" {
			continue
		}
		if tx.Ledger > latestLedger {
			latestLedger = tx.Ledger
		}

		ledgerClosedAt, _ := time.Parse(time.RFC3339, tx.CreatedAt)

		metaBytes, err := base64.StdEncoding.DecodeString(tx.ResultMetaXDR)
		if err != nil {
			// Skip transactions with unparseable meta
			continue
		}
		var meta xdr.TransactionMeta
		if err := meta.UnmarshalBinary(metaBytes); err != nil {
			continue
		}

		// Extract contract events from each operation
		for opIdx := uint32(0); opIdx < uint32(tx.OperationCount); opIdx++ {
			contractEvents, err := meta.GetContractEventsForOperation(opIdx)
			if err != nil {
				continue
			}
			for evIdx, ev := range contractEvents {
				if ev.ContractId == nil {
					continue
				}
				contractIDStr, err := contractIDToString(*ev.ContractId)
				if err != nil {
					continue
				}
				events = append(events, ContractEventFromMeta{
					Event:          &ev,
					ContractID:     contractIDStr,
					Ledger:         tx.Ledger,
					LedgerClosedAt: ledgerClosedAt,
					TxHash:         tx.Hash,
					TxIndex:        0, // Horizon doesn't easily expose this
					OpIndex:        opIdx,
					EventIndex:     uint32(evIdx),
				})
			}
		}
	}

	nextCursor := ""
	if tr.Links.Next.Href != "" {
		nextURL, err := url.Parse(tr.Links.Next.Href)
		if err == nil {
			nextCursor = nextURL.Query().Get("cursor")
		}
	}

	return events, nextCursor, latestLedger, nil
}

// GetLatestLedger fetches the latest ledger from Horizon's root endpoint.
func (c *Client) GetLatestLedger(ctx context.Context) (uint32, error) {
	u := c.baseURL + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("horizon root status %d: %.200s", res.StatusCode, string(body))
	}
	var root struct {
		CoreLatestLedger uint32 `json:"core_latest_ledger"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return 0, err
	}
	return root.CoreLatestLedger, nil
}

// GetHealth returns health info for the api.HealthChecker interface.
func (c *Client) GetHealth(ctx context.Context) (*stellar.Health, error) {
	latest, err := c.GetLatestLedger(ctx)
	if err != nil {
		return nil, err
	}
	return &stellar.Health{
		Status:       "healthy",
		LatestLedger: latest,
	}, nil
}

// OldestLedger returns 0 (unknown) as Horizon retention varies by deployment.
func (c *Client) OldestLedger(ctx context.Context) (uint32, error) {
	return 0, nil
}

// eventToStellarEvent converts a ContractEventFromMeta to a stellar.Event
// compatible with the existing decoder pipeline.
func eventToStellarEvent(ev ContractEventFromMeta) stellar.Event {
	topicB64 := make([]string, 0, len(ev.Event.Body.V0.Topics))
	for _, topic := range ev.Event.Body.V0.Topics {
		b, _ := topic.MarshalBinary()
		topicB64 = append(topicB64, base64.StdEncoding.EncodeToString(b))
	}
	valueB64 := ""
	if ev.Event.Body.V0.Data.Type != xdr.ScValTypeScvVoid {
		b, _ := ev.Event.Body.V0.Data.MarshalBinary()
		valueB64 = base64.StdEncoding.EncodeToString(b)
	}

	// Generate a deterministic ID from ledger, tx hash, op index, event index
	// Format: ledger (10 digits) + tx hash prefix + op + event
	id := fmt.Sprintf("%010d-%s-%d-%d", ev.Ledger, ev.TxHash[:8], ev.OpIndex, ev.EventIndex)

	return stellar.Event{
		ID:                 id,
		Type:               "contract",
		Ledger:             ev.Ledger,
		LedgerClosedAt:     ev.LedgerClosedAt,
		ContractID:         ev.ContractID,
		Topic:              topicB64,
		Value:              valueB64,
		TxHash:             ev.TxHash,
		InSuccessfulContractCall: true,
	}
}

// DecodeEvent uses the provided decoder to decode a ContractEventFromMeta.
func DecodeEvent(ctx context.Context, decoder stellar.Decoder, ev ContractEventFromMeta) (*stellar.DecodedEvent, error) {
	se := eventToStellarEvent(ev)
	return decoder.DecodeEvent(ctx, se)
}

// contractIDToString converts an xdr.ContractId to its strkey representation.
func contractIDToString(id xdr.ContractId) (string, error) {
	return strkey.Encode(strkey.VersionByteContract, id[:])
}