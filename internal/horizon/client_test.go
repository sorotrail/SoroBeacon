package horizon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Horizon retention varies by deployment, so the client does not ask the
// server: it returns 0, which callers must read as "unknown" rather than
// "everything is available". It must do so without any network access, which
// the failing server proves.
func TestClientOldestLedgerIsUnknownAndOffline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("OldestLedger must not call upstream, got %s", r.URL.Path)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	oldest, err := NewClient(srv.URL, srv.Client()).OldestLedger(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint32(0), oldest, "zero means unknown, not 'all ledgers retained'")
}

// Source delegates to the client, so the two implementations agree.
func TestSourceOldestLedgerMatchesClient(t *testing.T) {
	client := NewClient("http://127.0.0.1:0", nil)
	want, err := client.OldestLedger(context.Background())
	require.NoError(t, err)

	got, err := NewSource(client, nil).OldestLedger(context.Background())
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestContractIDToStringRoundTripsStrkey(t *testing.T) {
	var id xdr.ContractId
	for i := range id {
		id[i] = byte(i + 1)
	}

	got, err := contractIDToString(id)
	require.NoError(t, err)

	assert.Len(t, got, 56)
	assert.Equal(t, "C", got[:1])
	raw, err := strkey.Decode(strkey.VersionByteContract, got)
	require.NoError(t, err)
	assert.Equal(t, id[:], raw)
}

func TestContractIDToStringDistinguishesIDs(t *testing.T) {
	var a, b xdr.ContractId
	b[31] = 1

	sa, err := contractIDToString(a)
	require.NoError(t, err)
	sb, err := contractIDToString(b)
	require.NoError(t, err)
	assert.NotEqual(t, sa, sb)
}
