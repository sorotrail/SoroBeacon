package rules_test

import (
	"encoding/json"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/stretchr/testify/require"
)

func TestParseGuidanceHappyPath(t *testing.T) {
	params := json.RawMessage(`{
		"runbook_url": "https://runbooks.example.com/token/transfer",
		"response_guidance": "Confirm whether the sender is a known treasury account."
	}`)

	runbookURL, responseGuidance, err := rules.ParseGuidance(params)
	require.NoError(t, err)
	require.Equal(t, "https://runbooks.example.com/token/transfer", runbookURL)
	require.Equal(t, "Confirm whether the sender is a known treasury account.", responseGuidance)
}

func TestParseGuidanceErrorPath(t *testing.T) {
	// Invalid runbook URL scheme
	params := json.RawMessage(`{
		"runbook_url": "javascript:alert(1)",
		"response_guidance": "some guidance"
	}`)

	_, _, err := rules.ParseGuidance(params)
	require.Error(t, err)
}

func TestParseResponseGuidanceHappyPath(t *testing.T) {
	params := json.RawMessage(`{
		"response_guidance": "Confirm whether the sender is a known treasury account."
	}`)

	responseGuidance, err := rules.ParseResponseGuidance(params)
	require.NoError(t, err)
	require.Equal(t, "Confirm whether the sender is a known treasury account.", responseGuidance)
}

func TestParseResponseGuidanceErrorPath(t *testing.T) {
	// Non-string value for response_guidance should return an error
	params := json.RawMessage(`{
		"response_guidance": 12345
	}`)

	_, err := rules.ParseResponseGuidance(params)
	require.Error(t, err)
}

func TestParseGuidanceEmptyParams(t *testing.T) {
	// Empty params should return empty strings with no error
	runbookURL, responseGuidance, err := rules.ParseGuidance(json.RawMessage(""))
	require.NoError(t, err)
	require.Equal(t, "", runbookURL)
	require.Equal(t, "", responseGuidance)
}

func TestParseResponseGuidanceEmptyParams(t *testing.T) {
	// Empty params should return empty string with no error
	responseGuidance, err := rules.ParseResponseGuidance(json.RawMessage(""))
	require.NoError(t, err)
	require.Equal(t, "", responseGuidance)
}

func TestParseGuidanceNullValue(t *testing.T) {
	// Null runbook_url value should be treated as absent
	params := json.RawMessage(`{
		"runbook_url": null,
		"response_guidance": "some guidance"
	}`)

	runbookURL, responseGuidance, err := rules.ParseGuidance(params)
	require.NoError(t, err)
	require.Equal(t, "", runbookURL)
	require.Equal(t, "some guidance", responseGuidance)
}