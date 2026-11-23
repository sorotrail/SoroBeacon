package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// A request for monitor 0 is a mistake, and the store's "zero means no
// filter" convention would answer it with every alert on the instance. The
// filter parser refuses it instead.
func TestAlertFilterRejectsANonPositiveID(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "sorobeacon.db")
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	srv := httptest.NewServer(authServerOn(st, nil))
	defer srv.Close()

	for _, q := range []string{"monitor_id=0", "monitor_id=-1", "rule_id=0", "rule_id=-3"} {
		res, err := http.Get(srv.URL + "/alerts?" + q)
		require.NoError(t, err)
		body := readBody(t, res)
		assert.Equal(t, http.StatusBadRequest, res.StatusCode, "GET /alerts?%s", q)
		assert.Contains(t, body, "invalid", "GET /alerts?%s: %s", q, body)
	}
}

// readBody reads a response body as a string.
func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return out.String()
}

// A federated alert is exported with the instance it came from, so an
// operator reading the export can tell it from a local alert.
func TestAlertExportNamesTheOriginOfAFederatedAlert(t *testing.T) {
	fed := store.Alert{
		ID:               7,
		EventID:          "ev-1",
		Federated:        true,
		FederationOrigin: "peer-a",
		SourceRuleID:     3,
		CreatedAt:        store.Alert{}.CreatedAt,
	}
	row := alertCSVRow(fed, "")
	assert.Equal(t, "federated:peer-a", row[1], "the monitor column names the origin")
	assert.Equal(t, "3", row[2], "the rule column carries the sender's rule id")

	nd := exportAlertNDJSONRow(fed, "")
	assert.Equal(t, true, nd["federated"])
	assert.Equal(t, "peer-a", nd["federation_origin"])
	assert.Equal(t, "3", nd["source_rule_id"])
	assert.Equal(t, "federated:peer-a", nd["monitor_name"])
}

// The JSON body of an alert carries the provenance too, so a client that
// reads /alerts can tell a forwarded alert from a local one.
func TestAlertJSONCarriesTheProvenance(t *testing.T) {
	b, err := json.Marshal(store.Alert{
		ID:               7,
		Federated:        true,
		FederationOrigin: "peer-a",
		SourceMonitorID:  42,
		SourceRuleID:     3,
	})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"federated":true`)
	assert.Contains(t, string(b), `"federation_origin":"peer-a"`)
	assert.Contains(t, string(b), `"source_monitor_id":42`)
	assert.Contains(t, string(b), `"source_rule_id":3`)
}
