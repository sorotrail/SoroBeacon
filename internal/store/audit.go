package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Audit actor classifications persisted on audit_log.actor_type.
const (
	AuditActorPerson     = "person"
	AuditActorAutomation = "automation"
	AuditActorSystem     = "system"
	AuditActorAnonymous  = "anonymous"
)

// Audit surfaces persisted on audit_log.surface.
const (
	AuditSurfaceREST      = "rest"
	AuditSurfaceGraphQL   = "graphql"
	AuditSurfaceGRPC      = "grpc"
	AuditSurfaceDashboard = "dashboard"
	AuditSurfaceSystem    = "system"
)

// Audit outcomes persisted on audit_log.outcome.
const (
	AuditOutcomeSucceeded = "succeeded"
	AuditOutcomeFailed    = "failed"
)

// Audit target resource types persisted on audit_log.target_type.
const (
	AuditTargetMonitor            = "monitor"
	AuditTargetRule               = "rule"
	AuditTargetChannel            = "channel"
	AuditTargetAlert              = "alert"
	AuditTargetToken              = "token"
	AuditTargetDeadLetter         = "dead_letter"
	AuditTargetMaintenanceWindow  = "maintenance_window"
	AuditTargetInhibition         = "inhibition"
	AuditTargetTemplate           = "template"
	AuditTargetEscalationPolicy   = "escalation_policy"
	AuditTargetSavedSearch        = "saved_search"
	AuditTargetPersistedOperation = "persisted_operation"
	AuditTargetQuota              = "quota"
	AuditTargetWorkspace          = "workspace"
	AuditTargetSession            = "session"
)

// ValidAuditActorType reports whether s is a supported audit actor_type value.
func ValidAuditActorType(s string) bool {
	switch s {
	case AuditActorPerson, AuditActorAutomation, AuditActorSystem, AuditActorAnonymous:
		return true
	default:
		return false
	}
}

// ValidAuditSurface reports whether s is a supported audit surface value.
func ValidAuditSurface(s string) bool {
	switch s {
	case AuditSurfaceREST, AuditSurfaceGraphQL, AuditSurfaceGRPC, AuditSurfaceDashboard, AuditSurfaceSystem:
		return true
	default:
		return false
	}
}

// ValidAuditOutcome reports whether s is a supported audit outcome value.
func ValidAuditOutcome(s string) bool {
	switch s {
	case AuditOutcomeSucceeded, AuditOutcomeFailed:
		return true
	default:
		return false
	}
}

// ValidAuditTargetType reports whether s is a known audit target_type filter value.
func ValidAuditTargetType(s string) bool {
	switch s {
	case "monitor", "rule", "channel", "alert", "token", "dead_letter",
		"maintenance_window", "inhibition", "template", "escalation_policy",
		"saved_search", "persisted_operation", "quota", "workspace",
		"workspace_retention", "retention_policy",
		"session", "alert_partition":
		return true
	default:
		return false
	}
}

// sensitiveAuditKeys lists JSON keys whose values must never be persisted in
// audit_log.diff. Field names inside the "fields" array are allowed; raw values
// under these keys are always stripped.
var sensitiveAuditKeys = map[string]bool{
	"config":          true,
	"secret":          true,
	"token":           true,
	"api_token":       true,
	"webhook_url":     true,
	"bot_token":       true,
	"password":        true,
	"smtp_password":   true,
	"routing_key":     true,
	"api_key":         true,
	"signing_secret":  true,
	"client_secret":   true,
	"authorization":   true,
	"headers":         true,
	"secret_key":      true,
	"private_key":     true,
	"bearer":          true,
	"session":         true,
	"sorobeacon_conf": true,
}

// NormalizeAuditEntry populates defaults on e from ctx and sanitizes e.Diff so
// secret material (such as channel config or API token values) never reaches
// the database.
func NormalizeAuditEntry(ctx context.Context, e *AuditEntry) {
	if e == nil {
		return
	}
	if strings.TrimSpace(e.WorkspaceID) == "" {
		e.WorkspaceID = string(workspaceID(ctx))
	}
	e.Actor = strings.TrimSpace(e.Actor)
	if strings.TrimSpace(e.ActorType) == "" {
		switch {
		case strings.HasPrefix(e.Actor, "system:") || e.Actor == "partition_lifecycle":
			e.ActorType = AuditActorSystem
		case e.Actor == "" || e.Actor == AuditActorAnonymous:
			e.ActorType = AuditActorAnonymous
		default:
			e.ActorType = AuditActorPerson
		}
	}
	if e.Actor == "" {
		e.Actor = AuditActorAnonymous
	}
	if strings.TrimSpace(e.Surface) == "" {
		if e.ActorType == AuditActorSystem {
			e.Surface = AuditSurfaceSystem
		} else {
			e.Surface = AuditSurfaceREST
		}
	}
	if strings.TrimSpace(e.Outcome) == "" {
		if e.StatusCode >= 400 {
			e.Outcome = AuditOutcomeFailed
		} else {
			e.Outcome = AuditOutcomeSucceeded
		}
	}
	if e.StatusCode == 0 {
		if e.Outcome == AuditOutcomeFailed {
			e.StatusCode = 400
		} else {
			e.StatusCode = 200
		}
	}
	e.Diff = SanitizeAuditDiff(e.TargetType, e.Diff)
}

// SanitizeAuditDiff strips any secret or channel config values from raw and
// returns canonical compact JSON (defaulting to `{}`).
func SanitizeAuditDiff(targetType string, raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{}`)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return json.RawMessage(`{}`)
	}

	cleaned := sanitizeAuditMap(obj)
	out, err := json.Marshal(cleaned)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(out)
}

func sanitizeAuditMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		lower := strings.ToLower(strings.TrimSpace(k))
		if lower == "fields" {
			out[k] = v
			continue
		}
		if sensitiveAuditKeys[lower] || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") {
			continue
		}
		switch val := v.(type) {
		case map[string]any:
			out[k] = sanitizeAuditMap(val)
		case string:
			if looksLikeSecretValue(val) {
				continue
			}
			out[k] = val
		default:
			out[k] = v
		}
	}
	return out
}

func looksLikeSecretValue(s string) bool {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "sb_") && len(trimmed) > 12 {
		return true
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "bearer ") || strings.Contains(lower, "hooks.slack.com") || strings.Contains(lower, "discord.com/api/webhooks")
}

// canonicalAuditDiff produces a deterministic compact JSON representation so
// Postgres JSONB and SQLite TEXT columns hash identically.
func canonicalAuditDiff(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ComputeAuditRecordHash returns the hex-encoded SHA-256 tamper-evidence hash
// for e, chaining from e.PrevHash and covering every audit metadata column.
func ComputeAuditRecordHash(e AuditEntry) string {
	ts := ""
	if !e.CreatedAt.IsZero() {
		ts = e.CreatedAt.UTC().Truncate(time.Millisecond).Format(sqliteTimeLayout)
	}
	payload := fmt.Sprintf(
		"v1|%s|%s|%s|%s|%s|%s|%d|%s|%s|%s|%s|%d|%s|%s|%s",
		e.PrevHash,
		e.WorkspaceID,
		e.Actor,
		e.ActorType,
		e.Action,
		e.TargetType,
		e.TargetID,
		e.ResourceID,
		e.Surface,
		e.SourceIP,
		e.Outcome,
		e.StatusCode,
		e.Reason,
		canonicalAuditDiff(e.Diff),
		ts,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// VerifyAuditHashChain verifies the tamper-evidence hash chain for a slice of
// audit entries (in either ascending or descending ID order). It returns
// (true, -1, nil) when every entry's RecordHash matches its content and links
// to the previous entry in the same workspace chain, or (false, index, err)
// identifying the first broken entry in ascending ID order.
func VerifyAuditHashChain(entries []AuditEntry) (bool, int, error) {
	if len(entries) == 0 {
		return true, -1, nil
	}
	sorted := make([]AuditEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})

	lastHashByWorkspace := make(map[string]string)
	for i, e := range sorted {
		if e.RecordHash == "" {
			return false, i, fmt.Errorf("audit entry %d has empty record_hash", e.ID)
		}
		ws := e.WorkspaceID
		if ws == "" {
			ws = "default"
		}
		if prev, seen := lastHashByWorkspace[ws]; seen {
			if e.PrevHash != prev {
				return false, i, fmt.Errorf("audit entry %d prev_hash mismatch: got %q, want %q", e.ID, e.PrevHash, prev)
			}
		}
		want := ComputeAuditRecordHash(e)
		if e.RecordHash != want {
			return false, i, fmt.Errorf("audit entry %d record_hash mismatch: got %q, want %q", e.ID, e.RecordHash, want)
		}
		lastHashByWorkspace[ws] = e.RecordHash
	}
	return true, -1, nil
}
