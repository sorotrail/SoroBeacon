package stellar

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixtures ---------------------------------------------------------------

// countingSource is a SpecSource whose answer is scripted, so tests can assert
// how often it was consulted.
type countingSource struct {
	mu    sync.Mutex
	calls int
	spec  *ContractSpec
	err   error
}

func (s *countingSource) ContractSpec(context.Context, string) (*ContractSpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.spec, s.err
}

func (s *countingSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func transferJSONEvent() Event {
	return Event{
		ID:         "e1",
		ContractID: "CCONTRACT",
		TopicJSON: []json.RawMessage{
			json.RawMessage(`{"symbol": "transfer"}`),
			json.RawMessage(`{"address": "GFROM"}`),
			json.RawMessage(`{"address": "GTO"}`),
		},
		ValueJSON: json.RawMessage(`{"map": [{"key": {"symbol": "amount"}, "val": {"i128": "42"}}]}`),
	}
}

func newTestSpecDecoder(src SpecSource, clock *time.Time) *SpecDecoder {
	d := NewSpecDecoder(DefaultDecoder{}, src, slog.New(slog.DiscardHandler))
	d.withClock(func() time.Time { return *clock })
	return d
}

// withClock is the test hook for the TTL clock (the production default is
// time.Now); keeping it unexported avoids widening the API just for tests.
func (d *SpecDecoder) withClock(now func() time.Time) {
	d.now = now
}

// --- SpecDecoder ------------------------------------------------------------

func TestSpecDecoderNamedDecoding(t *testing.T) {
	src := &countingSource{spec: transferSpec(t)}
	dec := NewSpecDecoder(DefaultDecoder{}, src, slog.New(slog.DiscardHandler))

	decoded, err := dec.DecodeEvent(context.Background(), transferJSONEvent())
	require.NoError(t, err)
	require.NotNil(t, decoded.Fields)
	assert.Equal(t, "GFROM", decoded.Fields["from"])
	assert.Equal(t, "GTO", decoded.Fields["to"])
	assert.Equal(t, big.NewInt(42), decoded.Fields["amount"])
	// Positional decoding is unchanged.
	assert.Equal(t, []any{"transfer", "GFROM", "GTO"}, decoded.Topics)
}

func TestSpecDecoderFallsBackWithoutSpec(t *testing.T) {
	src := &countingSource{err: ErrNoSpec}
	dec := NewSpecDecoder(DefaultDecoder{}, src, slog.New(slog.DiscardHandler))

	decoded, err := dec.DecodeEvent(context.Background(), transferJSONEvent())
	require.NoError(t, err)
	assert.Nil(t, decoded.Fields, "no spec must leave the default decoding untouched")

	// Compare against the wrapped decoder to prove byte-for-byte equivalence.
	want, err := DefaultDecoder{}.DecodeEvent(context.Background(), transferJSONEvent())
	require.NoError(t, err)
	assert.Equal(t, want, decoded)
}

func TestSpecDecoderFallsBackOnFetchError(t *testing.T) {
	src := &countingSource{err: errors.New("rpc unavailable")}
	dec := NewSpecDecoder(DefaultDecoder{}, src, slog.New(slog.DiscardHandler))

	decoded, err := dec.DecodeEvent(context.Background(), transferJSONEvent())
	require.NoError(t, err, "a failed spec fetch must not fail the event")
	assert.Nil(t, decoded.Fields)

	want, err := DefaultDecoder{}.DecodeEvent(context.Background(), transferJSONEvent())
	require.NoError(t, err)
	assert.Equal(t, want, decoded)
}

func TestSpecDecoderCachesSpec(t *testing.T) {
	clock := time.Unix(0, 0)
	src := &countingSource{spec: transferSpec(t)}
	dec := newTestSpecDecoder(src, &clock)
	dec.WithTTL(time.Minute, time.Second)

	for i := 0; i < 3; i++ {
		decoded, err := dec.DecodeEvent(context.Background(), transferJSONEvent())
		require.NoError(t, err)
		require.NotNil(t, decoded.Fields)
	}
	assert.Equal(t, 1, src.callCount(), "a cached spec must be fetched once")

	// After the positive TTL the spec is refetched.
	clock = clock.Add(2 * time.Minute)
	_, err := dec.DecodeEvent(context.Background(), transferJSONEvent())
	require.NoError(t, err)
	assert.Equal(t, 2, src.callCount())
}

func TestSpecDecoderCachesMissingSpec(t *testing.T) {
	clock := time.Unix(0, 0)
	src := &countingSource{err: ErrNoSpec}
	dec := newTestSpecDecoder(src, &clock)
	dec.WithTTL(time.Minute, time.Second)

	for i := 0; i < 3; i++ {
		decoded, err := dec.DecodeEvent(context.Background(), transferJSONEvent())
		require.NoError(t, err)
		assert.Nil(t, decoded.Fields)
	}
	assert.Equal(t, 1, src.callCount(), "a contract with no spec must not be refetched per event")

	// Negative results expire sooner, so a contract that gains a spec later is
	// picked up without a restart.
	clock = clock.Add(2 * time.Second)
	_, err := dec.DecodeEvent(context.Background(), transferJSONEvent())
	require.NoError(t, err)
	assert.Equal(t, 2, src.callCount())
}

func TestSpecDecoderCachesFetchError(t *testing.T) {
	src := &countingSource{err: errors.New("rpc unavailable")}
	dec := NewSpecDecoder(DefaultDecoder{}, src, slog.New(slog.DiscardHandler))
	dec.WithTTL(time.Minute, time.Minute)

	for i := 0; i < 3; i++ {
		_, err := dec.DecodeEvent(context.Background(), transferJSONEvent())
		require.NoError(t, err)
	}
	assert.Equal(t, 1, src.callCount(), "a failing fetch must be cached to avoid hammering the RPC")
}

// --- RPCSpecSource ----------------------------------------------------------

// fakeLedgerClient serves scripted getLedgerEntries pages.
type fakeLedgerClient struct {
	mu        sync.Mutex
	responses [][]LedgerEntryResult
	calls     int
}

func (f *fakeLedgerClient) GetLedgerEntries(context.Context, []string) ([]LedgerEntryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.responses) == 0 {
		return nil, nil
	}
	res := f.responses[0]
	f.responses = f.responses[1:]
	return res, nil
}

func contractIDForTest(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	for i := range raw {
		raw[i] = seed
	}
	id, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	require.NoError(t, err)
	return id
}

func encodeEntry(t *testing.T, data xdr.LedgerEntryData) LedgerEntryResult {
	t.Helper()
	b64, err := xdr.MarshalBase64(data)
	require.NoError(t, err)
	return LedgerEntryResult{DataXDR: b64}
}

func instanceResult(t *testing.T, executable xdr.ContractExecutable) LedgerEntryResult {
	t.Helper()
	instance := xdr.ScContractInstance{Executable: executable}
	contract := xdr.ContractId(xdr.Hash{})
	return encodeEntry(t, xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &instance},
		},
	})
}

func TestRPCSpecSourceFetchesAndParses(t *testing.T) {
	wasm := wasmWithSpec(t, []xdr.ScSpecEntry{
		eventEntry(t, []string{"transfer"}, []xdr.ScSpecEventParamV0{topicParam("from"), dataParam("amount")}, xdr.ScSpecEventDataFormatScSpecEventDataFormatMap),
	})
	hash := xdr.Hash{0xAB}
	client := &fakeLedgerClient{responses: [][]LedgerEntryResult{
		{instanceResult(t, xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &hash})},
		{encodeEntry(t, xdr.LedgerEntryData{
			Type:         xdr.LedgerEntryTypeContractCode,
			ContractCode: &xdr.ContractCodeEntry{Hash: hash, Code: wasm},
		})},
	}}

	spec, err := NewRPCSpecSource(client).ContractSpec(context.Background(), contractIDForTest(t, 0x11))
	require.NoError(t, err)
	require.NotNil(t, spec)
	require.Len(t, spec.Events, 1)
	assert.Equal(t, []string{"transfer"}, spec.Events[0].PrefixTopics)
	assert.Equal(t, 2, client.calls, "the instance and the Wasm are separate ledger entries")
}

func TestRPCSpecSourceStellarAssetContractHasNoSpec(t *testing.T) {
	client := &fakeLedgerClient{responses: [][]LedgerEntryResult{
		{instanceResult(t, xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset})},
	}}
	_, err := NewRPCSpecSource(client).ContractSpec(context.Background(), contractIDForTest(t, 0x22))
	assert.ErrorIs(t, err, ErrNoSpec)
}

func TestRPCSpecSourceUnknownContractHasNoSpec(t *testing.T) {
	client := &fakeLedgerClient{responses: [][]LedgerEntryResult{{}}}
	_, err := NewRPCSpecSource(client).ContractSpec(context.Background(), contractIDForTest(t, 0x33))
	assert.ErrorIs(t, err, ErrNoSpec)
}

func TestRPCSpecSourceRejectsInvalidContractID(t *testing.T) {
	client := &fakeLedgerClient{}
	_, err := NewRPCSpecSource(client).ContractSpec(context.Background(), "not-a-contract")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoSpec))
}
