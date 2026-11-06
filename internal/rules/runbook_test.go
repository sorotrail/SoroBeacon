package rules_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/rules"
)

func TestRegistryValidateRunbookAndGuidanceInParams(t *testing.T) {
	t.Parallel()

	reg := rules.NewRegistry()

	valid := json.RawMessage(`{
		"event_name": "transfer",
		"runbook_url": "https://runbooks.example.com/token/transfer",
		"response_guidance": "Confirm whether the sender is a known treasury account."
	}`)
	if err := reg.Validate("event_emitted", valid); err != nil {
		t.Fatalf("Validate valid params with runbook_url & response_guidance: %v", err)
	}

	invalidScheme := json.RawMessage(`{
		"event_name": "transfer",
		"runbook_url": "javascript:alert(1)"
	}`)
	if err := reg.Validate("event_emitted", invalidScheme); err == nil {
		t.Fatal("Validate javascript: runbook_url succeeded, want error")
	}

	invalidType := json.RawMessage(`{
		"event_name": "transfer",
		"runbook_url": 12345
	}`)
	if err := reg.Validate("event_emitted", invalidType); err == nil {
		t.Fatal("Validate non-string runbook_url succeeded, want error")
	}

	tooLongGuidance := json.RawMessage(`{
		"event_name": "transfer",
		"response_guidance": "` + strings.Repeat("g", rules.MaxResponseGuidanceLen+1) + `"
	}`)
	if err := reg.Validate("event_emitted", tooLongGuidance); err == nil {
		t.Fatal("Validate oversized response_guidance succeeded, want error")
	}
}
