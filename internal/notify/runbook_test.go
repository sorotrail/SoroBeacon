package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderTextIncludesRunbookAndGuidance(t *testing.T) {
	t.Parallel()

	a := Alert{
		MonitorName:      "Treasury Watch",
		RuleType:         "value_threshold",
		EventName:        "transfer",
		ContractID:       "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		Ledger:           42,
		TxHash:           "deadbeef",
		RunbookURL:       "https://runbooks.example.com/soroban/treasury",
		ResponseGuidance: "1. Check signers.\n2. Pause bridge if unauthorized.",
	}
	msg, err := RenderText(a)
	require.NoError(t, err)
	assert.Contains(t, msg, "Runbook: https://runbooks.example.com/soroban/treasury")
	assert.Contains(t, msg, "Guidance: 1. Check signers.\n2. Pause bridge if unauthorized.")
}

func TestChannelTemplateEscapingHelpers(t *testing.T) {
	t.Parallel()

	rawTmpl := `Slack: {{slackEscape .ResponseGuidance}} | HTML: {{htmlEscape .ResponseGuidance}} | JSON: "{{jsonEscape .ResponseGuidance}}" | MD: {{markdownEscape .ResponseGuidance}} | Runbook: {{.RunbookURL}}`
	tmpl, err := parseChannelTemplate(rawTmpl)
	require.NoError(t, err)

	a := Alert{
		RunbookURL:       "https://runbooks.example.com/rb",
		ResponseGuidance: `Check <!channel> & "pause" *now*`,
	}
	rendered, err := tmpl.render(a)
	require.NoError(t, err)
	assert.Contains(t, rendered, `Slack: Check &lt;!channel&gt; &amp; "pause" *now*`)
	assert.Contains(t, rendered, `HTML: Check &lt;!channel&gt; &amp; &#34;pause&#34; *now*`)
	assert.Contains(t, rendered, `JSON: "Check \u003c!channel\u003e \u0026 \"pause\" *now*"`)
	assert.Contains(t, rendered, `MD: Check &lt;!channel&gt; & "pause" \*now\*`)
	assert.Contains(t, rendered, `Runbook: https://runbooks.example.com/rb`)
}

func TestPagerDutyAndOpsgenieIncludeRunbookAndGuidance(t *testing.T) {
	t.Parallel()

	var pdBody map[string]any
	pdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &pdBody)
		fmt.Fprint(w, `{"status":"success"}`)
	}))
	defer pdSrv.Close()

	pd, err := NewPagerDuty(json.RawMessage(fmt.Sprintf(`{"routing_key":"rk","events_url":%q}`, pdSrv.URL)))
	require.NoError(t, err)

	alert := Alert{
		ID:               7,
		MonitorID:        2,
		MonitorName:      "Bridge Monitor",
		RuleID:           5,
		RuleType:         "event_emitted",
		EventID:          "ev-7",
		ContractID:       "CABC",
		RunbookURL:       "https://runbooks.example.com/bridge",
		ResponseGuidance: "Verify bridge guardian signatures.",
	}
	require.NoError(t, pd.Send(context.Background(), alert))

	payload, _ := pdBody["payload"].(map[string]any)
	details, _ := payload["custom_details"].(map[string]any)
	assert.Equal(t, "https://runbooks.example.com/bridge", details["runbook_url"])
	assert.Equal(t, "Verify bridge guardian signatures.", details["response_guidance"])
	links, _ := pdBody["links"].([]any)
	require.Len(t, links, 1)
	link0, _ := links[0].(map[string]any)
	assert.Equal(t, "https://runbooks.example.com/bridge", link0["href"])

	var ogBody map[string]any
	ogSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &ogBody)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"result":"Request will be processed"}`)
	}))
	defer ogSrv.Close()

	og, err := NewOpsgenie(json.RawMessage(fmt.Sprintf(`{"api_key":"og-key","api_base":%q}`, ogSrv.URL)))
	require.NoError(t, err)
	require.NoError(t, og.Send(context.Background(), alert))

	ogDetails, _ := ogBody["details"].(map[string]any)
	assert.Equal(t, "https://runbooks.example.com/bridge", ogDetails["runbook_url"])
	assert.Equal(t, "Verify bridge guardian signatures.", ogDetails["response_guidance"])
	desc, _ := ogBody["description"].(string)
	assert.Contains(t, desc, "Runbook: https://runbooks.example.com/bridge")
	assert.Contains(t, desc, "Guidance: Verify bridge guardian signatures.")
}
