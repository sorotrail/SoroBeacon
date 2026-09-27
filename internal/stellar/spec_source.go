package stellar

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ErrNoSpec reports that a contract has no exported event spec: it is absent,
// is a Stellar Asset Contract, or its Wasm carries no contractspecv0 section.
// Callers treat it as "use default decoding", not as a failure.
var ErrNoSpec = errors.New("contract has no event spec")

// SpecSource supplies a contract's exported spec. An implementation returns
// ErrNoSpec when the contract has none; any other error is a fetch failure
// that callers may retry. SpecDecoder caches the result either way.
//
// Contributors: implement this to source specs from somewhere other than the
// RPC (a local cache, an indexer) and wire it into NewSpecDecoder.
type SpecSource interface {
	ContractSpec(ctx context.Context, contractID string) (*ContractSpec, error)
}

// LedgerEntryClient is the slice of Stellar RPC a SpecSource needs: reading
// ledger entries by base64-encoded LedgerKey. HTTPClient implements it, and
// it is deliberately separate from Client so existing event sources and mocks
// are unaffected.
type LedgerEntryClient interface {
	GetLedgerEntries(ctx context.Context, keys []string) ([]LedgerEntryResult, error)
}

// LedgerEntryResult is one entry from getLedgerEntries. DataXDR decodes to an
// xdr.LedgerEntryData.
type LedgerEntryResult struct {
	KeyXDR             string  `json:"key,omitempty"`
	DataXDR            string  `json:"xdr,omitempty"`
	LastModifiedLedger uint32  `json:"lastModifiedLedgerSeq,omitempty"`
	LiveUntilLedgerSeq *uint32 `json:"liveUntilLedgerSeq,omitempty"`
}

// RPCSpecSource fetches a contract's spec from a Stellar RPC node. Soroban
// stores a contract's spec in a Wasm custom section, so this is two
// getLedgerEntries calls: first the contract instance (which names the Wasm
// hash), then the Wasm code itself.
type RPCSpecSource struct {
	entries LedgerEntryClient
}

// NewRPCSpecSource returns a SpecSource backed by a ledger-entry client.
func NewRPCSpecSource(entries LedgerEntryClient) *RPCSpecSource {
	return &RPCSpecSource{entries: entries}
}

var _ SpecSource = (*RPCSpecSource)(nil)

// ContractSpec fetches and parses contractID's event spec. A missing
// contract, a non-Wasm executable (the Stellar Asset Contract), or a Wasm
// without a spec section all yield ErrNoSpec so the caller falls back cleanly.
func (s *RPCSpecSource) ContractSpec(ctx context.Context, contractID string) (*ContractSpec, error) {
	address, err := contractScAddress(contractID)
	if err != nil {
		return nil, err
	}

	instance, err := s.fetchContractInstance(ctx, address)
	if err != nil {
		return nil, err
	}
	executable := instance.Executable
	if executable.Type != xdr.ContractExecutableTypeContractExecutableWasm || executable.WasmHash == nil {
		return nil, ErrNoSpec
	}

	wasm, err := s.fetchWasm(ctx, *executable.WasmHash)
	if err != nil {
		return nil, err
	}
	entries, err := specEntriesFromWasm(wasm)
	if err != nil {
		return nil, err
	}
	spec := ParseContractSpec(entries)
	if spec == nil {
		return nil, ErrNoSpec
	}
	return spec, nil
}

func (s *RPCSpecSource) fetchContractInstance(ctx context.Context, address xdr.ScAddress) (xdr.ScContractInstance, error) {
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract: address,
			// The contract instance lives under a well-known ledger key; its
			// executable names the Wasm to fetch next.
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	results, err := s.getEntries(ctx, key)
	if err != nil {
		return xdr.ScContractInstance{}, err
	}
	if len(results) == 0 {
		return xdr.ScContractInstance{}, ErrNoSpec
	}
	var data xdr.LedgerEntryData
	if err := xdr.SafeUnmarshalBase64(results[0].DataXDR, &data); err != nil {
		return xdr.ScContractInstance{}, fmt.Errorf("decode contract instance entry: %w", err)
	}
	contractData, ok := data.GetContractData()
	if !ok {
		return xdr.ScContractInstance{}, ErrNoSpec
	}
	instance, ok := contractData.Val.GetInstance()
	if !ok {
		return xdr.ScContractInstance{}, ErrNoSpec
	}
	return instance, nil
}

func (s *RPCSpecSource) fetchWasm(ctx context.Context, hash xdr.Hash) ([]byte, error) {
	key := xdr.LedgerKey{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.LedgerKeyContractCode{Hash: hash},
	}
	results, err := s.getEntries(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, ErrNoSpec
	}
	var data xdr.LedgerEntryData
	if err := xdr.SafeUnmarshalBase64(results[0].DataXDR, &data); err != nil {
		return nil, fmt.Errorf("decode contract code entry: %w", err)
	}
	code, ok := data.GetContractCode()
	if !ok || len(code.Code) == 0 {
		return nil, ErrNoSpec
	}
	return code.Code, nil
}

// getEntries marshals one LedgerKey and reads its single ledger entry.
func (s *RPCSpecSource) getEntries(ctx context.Context, key xdr.LedgerKey) ([]LedgerEntryResult, error) {
	encoded, err := xdr.MarshalBase64(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ledger key: %w", err)
	}
	return s.entries.GetLedgerEntries(ctx, []string{encoded})
}

// contractScAddress turns a contract strkey ("C...") into the ScAddress the
// ledger keys are built from.
func contractScAddress(contractID string) (xdr.ScAddress, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return xdr.ScAddress{}, fmt.Errorf("invalid contract id %q: %w", contractID, err)
	}
	var hash xdr.Hash
	copy(hash[:], raw)
	contract := xdr.ContractId(hash)
	return xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &contract,
	}, nil
}
