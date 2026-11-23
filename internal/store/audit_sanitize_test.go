package store

import (
	"encoding/json"
	"testing"
)

func TestSanitizeAuditDiff(t *testing.T) {
	tests := []struct {
		name       string
		targetType string
		raw        string
		want       string
	}{
		{
			name:       "keeps non-sensitive fields",
			targetType: AuditTargetMonitor,
			raw:        `{"name":"My token","enabled":true,"priority":"high"}`,
			want:       `{"name":"My token","enabled":true,"priority":"high"}`,
		},
		{
			name:       "drops a channel config object",
			targetType: AuditTargetChannel,
			raw:        `{"name":"ops","config":{"webhook_url":"https://example.test/hook"}}`,
			want:       `{"name":"ops"}`,
		},
		{
			name:       "drops keys that merely contain a secret word",
			targetType: AuditTargetChannel,
			raw:        `{"bot_token":"x","smtp_password":"y","note":"kept"}`,
			want:       `{"note":"kept"}`,
		},
		{
			name:       "drops secret-looking values",
			targetType: AuditTargetToken,
			raw:        `{"label":"ci","value":"sb_abcdefghijklmnop"}`,
			want:       `{"label":"ci"}`,
		},
		{
			name:       "sanitizes nested objects",
			targetType: AuditTargetChannel,
			raw:        `{"changes":{"config":{"x":1},"name":"renamed"}}`,
			want:       `{"changes":{"name":"renamed"}}`,
		},
		{
			name:       "preserves field names under fields",
			targetType: AuditTargetChannel,
			raw:        `{"fields":["config","token"]}`,
			want:       `{"fields":["config","token"]}`,
		},
		{
			name:       "empty input yields an empty object",
			targetType: AuditTargetMonitor,
			raw:        ``,
			want:       `{}`,
		},
		{
			name:       "whitespace-only input yields an empty object",
			targetType: AuditTargetMonitor,
			raw:        "  \n\t ",
			want:       `{}`,
		},
		{
			name:       "invalid JSON yields an empty object",
			targetType: AuditTargetMonitor,
			raw:        `{"name":`,
			want:       `{}`,
		},
		{
			name:       "non-object JSON yields an empty object",
			targetType: AuditTargetMonitor,
			raw:        `["name"]`,
			want:       `{}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeAuditDiff(tc.targetType, json.RawMessage(tc.raw))
			if !auditDiffEqual(t, got, tc.want) {
				t.Fatalf("SanitizeAuditDiff(%q, %s) = %s, want %s", tc.targetType, tc.raw, got, tc.want)
			}
		})
	}
}

// auditDiffEqual compares two JSON documents semantically so a test is about
// which keys survived rather than Go's map key ordering.
func auditDiffEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("SanitizeAuditDiff returned invalid JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("test expectation is not JSON: %v", err)
	}
	gotJSON, err := json.Marshal(gotValue)
	if err != nil {
		t.Fatalf("marshal sanitized diff: %v", err)
	}
	wantJSON, err := json.Marshal(wantValue)
	if err != nil {
		t.Fatalf("marshal expectation: %v", err)
	}
	return string(gotJSON) == string(wantJSON)
}
