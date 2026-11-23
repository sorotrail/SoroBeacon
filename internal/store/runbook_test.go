package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRunbookURLAndResponseGuidance(t *testing.T) {
	t.Parallel()

	u, err := ValidateRunbookURL("  https://runbooks.example.com/soroban/high-drain#step-1  ")
	require.NoError(t, err)
	require.Equal(t, "https://runbooks.example.com/soroban/high-drain#step-1", u)

	uEmpty, err := ValidateRunbookURL("   ")
	require.NoError(t, err)
	require.Equal(t, "", uEmpty)

	badURLs := []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"ftp://example.com/runbook",
		"/relative/path",
		"https://",
		"https://example.com/\x00bad",
		"https://example.com/" + strings.Repeat("a", MaxRunbookURLLen),
	}
	for _, raw := range badURLs {
		_, err := ValidateRunbookURL(raw)
		require.Error(t, err, "ValidateRunbookURL(%q) should fail", raw)
	}

	g, err := ValidateResponseGuidance("  1. Pause the bridge.\n2. Page treasury on-call.  ")
	require.NoError(t, err)
	require.Equal(t, "1. Pause the bridge.\n2. Page treasury on-call.", g)

	_, err = ValidateResponseGuidance("bad\x00null")
	require.Error(t, err)
	_, err = ValidateResponseGuidance(strings.Repeat("x", MaxResponseGuidanceLen+1))
	require.Error(t, err)
}

func TestResolveAlertGuidanceInheritance(t *testing.T) {
	t.Parallel()

	m := &Monitor{
		RunbookURL:       "https://runbooks.example.com/monitor-default",
		ResponseGuidance: "Monitor-level fallback guidance.",
	}
	r := &Rule{
		Type:             "event_emitted",
		Params:           json.RawMessage(`{"event_name":"transfer"}`),
		ResponseGuidance: "Rule-specific guidance.",
	}

	url, guidance := ResolveAlertGuidance(r, m, "https://runbooks.example.com/workspace", "Workspace fallback")
	require.Equal(t, "https://runbooks.example.com/monitor-default", url)
	require.Equal(t, "Rule-specific guidance.", guidance)

	rParams := &Rule{
		Type:   "event_emitted",
		Params: json.RawMessage(`{"event_name":"transfer","runbook_url":"https://runbooks.example.com/from-params"}`),
	}
	url2, guidance2 := ResolveAlertGuidance(rParams, nil, "https://runbooks.example.com/workspace", "Workspace fallback")
	require.Equal(t, "https://runbooks.example.com/from-params", url2)
	require.Equal(t, "Workspace fallback", guidance2)
}

func TestSQLiteRunbookAndGuidanceLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestSQLite(t)

	require.NoError(t, st.SetWorkspaceGuidance(ctx, "default", "https://runbooks.example.com/ws", "Escalation: #sec-ops"))
	wsURL, wsGuidance, err := st.GetWorkspaceGuidance(ctx, "default")
	require.NoError(t, err)
	require.Equal(t, "https://runbooks.example.com/ws", wsURL)
	require.Equal(t, "Escalation: #sec-ops", wsGuidance)

	m := &Monitor{
		Name:             "Treasury Watch",
		ContractIDs:      []string{"CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"},
		Enabled:          true,
		RunbookURL:       "https://runbooks.example.com/treasury",
		ResponseGuidance: "Check multisig signers before acknowledging.",
	}
	require.NoError(t, st.CreateMonitor(ctx, m))
	gotM, err := st.GetMonitor(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, m.RunbookURL, gotM.RunbookURL)
	require.Equal(t, m.ResponseGuidance, gotM.ResponseGuidance)

	rule := &Rule{
		MonitorID:        m.ID,
		Type:             "event_emitted",
		Params:           json.RawMessage(`{"event_name":"transfer"}`),
		Enabled:          true,
		ResponseGuidance: "Verify transfer recipient against allowlist.",
	}
	require.NoError(t, st.CreateRule(ctx, rule))
	gotR, err := st.GetRule(ctx, rule.ID)
	require.NoError(t, err)
	require.Equal(t, rule.ResponseGuidance, gotR.ResponseGuidance)

	alert := &Alert{
		MonitorID: m.ID,
		RuleID:    rule.ID,
		EventID:   "0001-1",
		Payload:   json.RawMessage(`{"contract_id":"CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA","event_name":"transfer"}`),
	}
	outcome, err := st.CreateAlert(ctx, alert)
	require.NoError(t, err)
	require.Equal(t, AlertCreated, outcome)

	gotA, err := st.GetAlert(ctx, alert.ID)
	require.NoError(t, err)
	require.Equal(t, m.RunbookURL, gotA.RunbookURL)
	require.Equal(t, rule.ResponseGuidance, gotA.ResponseGuidance)

	dup, err := st.DuplicateMonitor(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, m.RunbookURL, dup.RunbookURL)
	require.Equal(t, m.ResponseGuidance, dup.ResponseGuidance)

	dupRules, err := st.ListRules(ctx, dup.ID, false)
	require.NoError(t, err)
	require.Len(t, dupRules, 1)
	require.Equal(t, rule.ResponseGuidance, dupRules[0].ResponseGuidance)
}
