package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const maxI128 = "170141183460469231731687303715884105727"

func TestFormatDecimalKeepsWideIntegersExact(t *testing.T) {
	assert.Equal(t, "17014118346046923173168730371588.4105727", FormatDecimal(maxI128, 7))
	assert.Equal(t, "1.2345", FormatDecimal("12345000", 7))
	assert.Equal(t, "0.0000001", FormatDecimal("1", 7))
	assert.Equal(t, "-5", FormatDecimal("-50000000", 7))
	assert.Equal(t, "42", FormatDecimal("42", 0))
}

func enrichedAlert(enrichment, payload string) Alert {
	return Alert{ID: 1, MonitorName: "m", RuleType: "value_threshold", RuleID: 2, EventID: "e", Payload: json.RawMessage(payload), Enrichment: json.RawMessage(enrichment)}
}

func TestAmountKeepsExactIntegerBesideDecimals(t *testing.T) {
	a := enrichedAlert(`{"token_symbol":"USDC","token_decimals":7,"_source":"operator"}`, `{"value":`+maxI128+`}`)
	got := a.PrimaryAmount()
	assert.Contains(t, got, "exact integer "+maxI128)
	assert.Contains(t, got, "17014118346046923173168730371588.4105727 USDC")
	assert.Contains(t, got, "unverified metadata from operator")
}

func TestEnrichmentAbsentOrPartialDegradesToPlainOutput(t *testing.T) {
	plain, err := RenderText(enrichedAlert(``, `{"value":5}`))
	require.NoError(t, err)
	assert.NotContains(t, plain, "Label:")
	assert.NotContains(t, plain, "Amount:")

	// Label only: no decimals, so the amount is just the exact integer.
	partial, err := RenderText(enrichedAlert(`{"label":"Treasury","_source":"operator"}`, `{"value":5}`))
	require.NoError(t, err)
	assert.Contains(t, partial, "Label: Treasury (from operator; unverified)")
	assert.Contains(t, partial, "Amount: 5\n")

	// Garbage decimals are ignored rather than trusted.
	bad, err := RenderText(enrichedAlert(`{"token_decimals":9999,"token_symbol":"X"}`, `{"value":5}`))
	require.NoError(t, err)
	assert.Contains(t, bad, "Amount: 5")
	assert.NotContains(t, bad, "decimals")
}

func TestStaleEnrichmentIsLabelled(t *testing.T) {
	out, err := RenderText(enrichedAlert(`{"label":"Treasury","_source":"operator","_stale":true}`, `{}`))
	require.NoError(t, err)
	assert.Contains(t, out, "stale")
}

func TestHostileTokenSymbolIsEscapedPerChannel(t *testing.T) {
	hostile := `{"label":"<!channel> pay here","token_symbol":"U\u202eSDC<@U123>","token_decimals":7,"_source":"operator"}`
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		got, _ = m["text"].(string)
	}))
	defer srv.Close()
	n, err := NewSlack(json.RawMessage(`{"webhook_url":"` + srv.URL + `"}`))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), enrichedAlert(hostile, `{"value":12345000}`)))
	assert.NotContains(t, got, "<!channel>")
	assert.NotContains(t, got, "<@U123>")
	assert.NotContains(t, got, "\u202e", "bidi override characters are removed")
	assert.Contains(t, got, "&lt;!channel&gt; pay here")
	assert.Contains(t, got, "exact integer 12345000")
}
