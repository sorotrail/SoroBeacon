// Package python generates and verifies the Python client for the
// SoroBeacon HTTP API from internal/api/openapi.json.
//
//go:generate go test . -run TestPythonClientMatchesOpenAPISpec -update
package python

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// OpenAPIDoc represents the subset of OpenAPI 3.1 used by SoroBeacon's
// internal/api/openapi.json specification so the generator needs no external
// schema-loader dependencies.
type OpenAPIDoc struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]PathItem `json:"paths"`
	Components struct {
		Parameters map[string]Parameter         `json:"parameters"`
		Responses  map[string]ResponseSpec      `json:"responses"`
		Schemas    map[string]Schema            `json:"schemas"`
		Security   map[string]map[string]string `json:"securitySchemes"`
	} `json:"components"`
}

// PathItem holds the shared parameters and HTTP operations for a single path.
type PathItem struct {
	Parameters []Parameter `json:"parameters"`
	Get        *Operation  `json:"get"`
	Post       *Operation  `json:"post"`
	Put        *Operation  `json:"put"`
	Patch      *Operation  `json:"patch"`
	Delete     *Operation  `json:"delete"`
}

// Operation describes one HTTP method on a path.
type Operation struct {
	Summary     string                  `json:"summary"`
	Description string                  `json:"description"`
	Parameters  []Parameter             `json:"parameters"`
	RequestBody *RequestBody            `json:"requestBody"`
	Responses   map[string]ResponseSpec `json:"responses"`
}

// Parameter describes a path, query, or header parameter (or a $ref to one).
type Parameter struct {
	Ref         string `json:"$ref"`
	Name        string `json:"name"`
	In          string `json:"in"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
	Schema      Schema `json:"schema"`
}

// RequestBody describes the request payload of an operation.
type RequestBody struct {
	Required bool                   `json:"required"`
	Content  map[string]MediaObject `json:"content"`
}

// ResponseSpec describes a response status entry or a $ref to components.responses.
type ResponseSpec struct {
	Ref         string                 `json:"$ref"`
	Description string                 `json:"description"`
	Content     map[string]MediaObject `json:"content"`
}

// MediaObject wraps the schema for a media type such as application/json.
type MediaObject struct {
	Schema Schema `json:"schema"`
}

// Schema describes an OpenAPI JSON Schema object.
type Schema struct {
	Ref                  string            `json:"$ref"`
	Type                 string            `json:"type"`
	Format               string            `json:"format"`
	Description          string            `json:"description"`
	Enum                 []string          `json:"enum"`
	Required             []string          `json:"required"`
	Properties           map[string]Schema `json:"properties"`
	Items                *Schema           `json:"items"`
	AdditionalProperties *Schema           `json:"additionalProperties"`
}

// PackageDir returns the directory of this package on disk so tests and
// generators locate relative assets regardless of the working directory.
func PackageDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// DefaultSpecPath returns the path to internal/api/openapi.json.
func DefaultSpecPath() string {
	return filepath.Join(PackageDir(), "..", "..", "internal", "api", "openapi.json")
}

// DefaultOutputPath returns the path to the generated Python client entry point.
func DefaultOutputPath() string {
	return filepath.Join(PackageDir(), "sorobeacon", "__init__.py")
}

// GenerateFromFile reads internal/api/openapi.json and returns the deterministic
// Python source for clients/python/sorobeacon/__init__.py.
func GenerateFromFile(specPath string) ([]byte, error) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("read openapi spec %s: %w", specPath, err)
	}
	return Generate(raw)
}

type routeBinding struct {
	Method       string
	Path         string
	ClientMethod string
}

var knownRouteBindings = []routeBinding{
	{"GET", "/monitors", "list_monitors"},
	{"POST", "/monitors", "create_monitor"},
	{"POST", "/monitors/bulk", "bulk_update_monitors"},
	{"POST", "/monitors/import", "import_monitors"},
	{"GET", "/monitors/trash", "list_trash_monitors"},
	{"GET", "/monitors/{id}", "get_monitor"},
	{"PATCH", "/monitors/{id}", "update_monitor"},
	{"DELETE", "/monitors/{id}", "delete_monitor"},
	{"POST", "/monitors/{id}/soft-delete", "soft_delete_monitor"},
	{"POST", "/monitors/{id}/restore", "restore_monitor"},
	{"POST", "/monitors/{id}/duplicate", "duplicate_monitor"},
	{"GET", "/monitors/{id}/stats", "get_monitor_stats"},
	{"GET", "/monitors/{id}/escalation", "get_monitor_escalation"},
	{"PUT", "/monitors/{id}/escalation", "set_monitor_escalation"},
	{"DELETE", "/monitors/{id}/escalation", "delete_monitor_escalation"},
	{"GET", "/monitors/{id}/rules", "list_rules"},
	{"POST", "/monitors/{id}/rules", "create_rule"},
	{"POST", "/monitors/{id}/rules/bulk", "bulk_create_rules"},
	{"POST", "/monitors/{id}/rules/dry-run", "dry_run_rule"},
	{"PATCH", "/monitors/{id}/rules/{ruleID}", "update_rule"},
	{"DELETE", "/monitors/{id}/rules/{ruleID}", "delete_rule"},
	{"GET", "/channels", "list_channels"},
	{"POST", "/channels", "create_channel"},
	{"POST", "/channels/bulk", "bulk_update_channels"},
	{"GET", "/channels/{id}", "get_channel"},
	{"PATCH", "/channels/{id}", "update_channel"},
	{"DELETE", "/channels/{id}", "delete_channel"},
	{"GET", "/channels/{id}/stats", "get_channel_stats"},
	{"POST", "/channels/{id}/duplicate", "duplicate_channel"},
	{"POST", "/channels/{id}/test", "test_channel"},
	{"GET", "/channels/{id}/keys", "list_channel_keys"},
	{"POST", "/channels/{id}/keys/rotate", "rotate_channel_key"},
	{"POST", "/channels/{id}/keys/{keyID}/retire", "retire_channel_key"},
	{"POST", "/channels/{id}/keys/{keyID}/revoke", "revoke_channel_key"},
	{"GET", "/maintenance-windows", "list_maintenance_windows"},
	{"POST", "/maintenance-windows", "create_maintenance_window"},
	{"GET", "/maintenance-windows/{id}", "get_maintenance_window"},
	{"PATCH", "/maintenance-windows/{id}", "update_maintenance_window"},
	{"DELETE", "/maintenance-windows/{id}", "delete_maintenance_window"},
	{"GET", "/inhibitions", "list_inhibitions"},
	{"POST", "/inhibitions", "create_inhibition"},
	{"DELETE", "/inhibitions/{sourceID}/{targetID}", "delete_inhibition"},
	{"GET", "/alerts", "list_alerts"},
	{"GET", "/alerts/stream", "stream_alerts"},
	{"GET", "/alerts.csv", "export_alerts_csv"},
	{"GET", "/alerts/export", "export_alerts_ndjson"},
	{"POST", "/alerts/{id}/acknowledge", "acknowledge_alert"},
	{"GET", "/alerts/{id}/deliveries", "list_alert_deliveries"},
	{"POST", "/alerts/{id}/deliveries/{channelID}/retry", "retry_alert_delivery"},
	{"GET", "/dead-letters", "list_dead_letters"},
	{"GET", "/dead-letters/groups", "list_dead_letter_groups"},
	{"POST", "/dead-letters/bulk/redrive", "bulk_redrive_dead_letters"},
	{"POST", "/dead-letters/bulk/discard", "bulk_discard_dead_letters"},
	{"POST", "/dead-letters/{id}/redrive", "redrive_dead_letter"},
	{"POST", "/dead-letters/{id}/discard", "discard_dead_letter"},
	{"POST", "/ingest", "ingest_federated_alert"},
	{"POST", "/triggers", "trigger_evaluation"},
	{"GET", "/health", "get_health"},
	{"GET", "/livez", "get_livez"},
	{"GET", "/readyz", "get_readyz"},
	{"GET", "/poller", "get_poller_status"},
	{"GET", "/version", "get_version"},
	{"GET", "/rule-types", "list_rule_types"},
	{"GET", "/stats", "get_stats"},
	{"GET", "/stats/alerts-daily", "get_daily_alert_counts"},
	{"GET", "/audit", "list_audit_logs"},
	{"GET", "/templates", "list_templates"},
	{"POST", "/templates", "create_template"},
	{"GET", "/templates/{id}", "get_template"},
	{"PATCH", "/templates/{id}", "update_template"},
	{"DELETE", "/templates/{id}", "delete_template"},
	{"POST", "/templates/{id}/instantiate", "instantiate_template"},
	{"POST", "/templates/{id}/instantiate/bulk", "bulk_instantiate_template"},
	{"GET", "/tokens", "list_tokens"},
	{"POST", "/tokens", "create_token"},
	{"GET", "/tokens/{id}", "get_token"},
	{"POST", "/tokens/{id}/revoke", "revoke_token"},
	{"POST", "/tokens/{id}/rotate", "rotate_token"},
	{"POST", "/tokens/{id}/abandon", "abandon_token_rotation"},
	{"GET", "/graphql/operations", "list_graphql_operations"},
	{"POST", "/graphql/operations", "register_graphql_operation"},
	{"GET", "/graphql/operations/{id}", "get_graphql_operation"},
	{"DELETE", "/graphql/operations/{id}", "delete_graphql_operation"},
}

// Generate parses the OpenAPI JSON bytes, validates all component references and
// route mappings, and emits the complete Python SDK source.
func Generate(specBytes []byte) ([]byte, error) {
	var doc OpenAPIDoc
	if err := json.Unmarshal(specBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi.json: %w", err)
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("openapi.json contains no paths")
	}
	if len(doc.Components.Schemas) == 0 {
		return nil, fmt.Errorf("openapi.json contains no component schemas")
	}

	if err := validateRefsAndRoutes(&doc); err != nil {
		return nil, err
	}

	var b bytes.Buffer
	writeHeader(&b, &doc)
	writeScalarAndJSONTypes(&b)
	if err := writeComponentSchemas(&b, &doc); err != nil {
		return nil, err
	}
	writeSupplementaryEndpointTypes(&b)
	writeWideIntAndErrorRuntime(&b)
	writeRouteTable(&b, &doc)
	writeClientClass(&b)

	return b.Bytes(), nil
}

func validateRefsAndRoutes(doc *OpenAPIDoc) error {
	specOps := map[string]bool{}
	for path, item := range doc.Paths {
		if path == "/metrics" {
			continue
		}
		for _, p := range item.Parameters {
			if err := checkParamRef(doc, p.Ref); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		for method, op := range operationsMap(item) {
			if op == nil {
				continue
			}
			key := method + " " + path
			specOps[key] = true
			for _, p := range op.Parameters {
				if err := checkParamRef(doc, p.Ref); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
			}
			if op.RequestBody != nil {
				for _, media := range op.RequestBody.Content {
					if err := checkSchemaRefs(doc, media.Schema); err != nil {
						return fmt.Errorf("%s requestBody: %w", key, err)
					}
				}
			}
			for status, resp := range op.Responses {
				if resp.Ref != "" {
					name := strings.TrimPrefix(resp.Ref, "#/components/responses/")
					if _, ok := doc.Components.Responses[name]; !ok {
						return fmt.Errorf("%s response %s: unresolved $ref %q", key, status, resp.Ref)
					}
				}
				for _, media := range resp.Content {
					if err := checkSchemaRefs(doc, media.Schema); err != nil {
						return fmt.Errorf("%s response %s: %w", key, status, err)
					}
				}
			}
		}
	}

	for name, s := range doc.Components.Schemas {
		if err := checkSchemaRefs(doc, s); err != nil {
			return fmt.Errorf("components.schemas.%s: %w", name, err)
		}
	}

	boundOps := map[string]bool{}
	for _, rb := range knownRouteBindings {
		key := rb.Method + " " + rb.Path
		boundOps[key] = true
		if !specOps[key] {
			return fmt.Errorf("generator route binding %q (%s) is not present in openapi.json", rb.ClientMethod, key)
		}
	}
	for key := range specOps {
		if !boundOps[key] {
			return fmt.Errorf("openapi.json operation %q has no Python client method binding", key)
		}
	}
	return nil
}

func checkParamRef(doc *OpenAPIDoc, ref string) error {
	if ref == "" {
		return nil
	}
	name := strings.TrimPrefix(ref, "#/components/parameters/")
	if _, ok := doc.Components.Parameters[name]; !ok {
		return fmt.Errorf("unresolved parameter $ref %q", ref)
	}
	return nil
}

func checkSchemaRefs(doc *OpenAPIDoc, s Schema) error {
	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, "#/components/schemas/")
		if _, ok := doc.Components.Schemas[name]; !ok {
			return fmt.Errorf("unresolved schema $ref %q", s.Ref)
		}
	}
	for _, prop := range s.Properties {
		if err := checkSchemaRefs(doc, prop); err != nil {
			return err
		}
	}
	if s.Items != nil {
		if err := checkSchemaRefs(doc, *s.Items); err != nil {
			return err
		}
	}
	if s.AdditionalProperties != nil {
		if err := checkSchemaRefs(doc, *s.AdditionalProperties); err != nil {
			return err
		}
	}
	return nil
}

func operationsMap(item PathItem) map[string]*Operation {
	return map[string]*Operation{
		"GET":    item.Get,
		"POST":   item.Post,
		"PUT":    item.Put,
		"PATCH":  item.Patch,
		"DELETE": item.Delete,
	}
}

func writeHeader(b *bytes.Buffer, doc *OpenAPIDoc) {
	fmt.Fprintf(b, `# Code generated from internal/api/openapi.json (%s v%s). DO NOT EDIT.
# Regenerate with: go generate ./clients/python/...
# mypy: disable-error-code="no-any-return"
"""Generated Python client for the %s HTTP API (v%s)."""

from __future__ import annotations

import json
import re
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Iterator, Mapping, Sequence
from decimal import Decimal, InvalidOperation
from typing import Any, Literal, NotRequired, Required, TypeAlias, TypedDict

`, doc.Info.Title, doc.Info.Version, doc.Info.Title, doc.Info.Version)
}

func writeScalarAndJSONTypes(b *bytes.Buffer) {
	b.WriteString(`# Wide integer type aliases (resource IDs, ledger sequence numbers, counters,
# and Soroban stroop/i128/u128 amounts). Wide integers are typed as Decimal or
# str (and accept int on input), never float, because an IEEE-754 float64 only
# has 53 bits of significand and silently corrupts large integer values.
WideInt: TypeAlias = Decimal | str
Int64: TypeAlias = Decimal | str | int
Int64Value: TypeAlias = Decimal | str | int

# RFC 3339 / ISO-8601 timestamp string (e.g. "2026-01-15T12:34:56Z").
ISODateTimeString: TypeAlias = str

# Calendar date string in YYYY-MM-DD format (UTC).
ISODateString: TypeAlias = str

# Structured JSON value types (never an opaque unparsed string). Nested values
# use Any at the recursive leaf so runtime type introspection (get_type_hints)
# and static checkers both succeed without recursive TypeAlias cycles.
JsonPrimitive: TypeAlias = str | int | Decimal | bool | None
JsonValue: TypeAlias = JsonPrimitive | dict[str, Any] | list[Any]
JsonObject: TypeAlias = dict[str, Any]


class AlertPayload(TypedDict, total=False):
    """Decoded Soroban contract event payload attached to an Alert.

    Large integers inside decoded topics or event values (> 2^53 - 1, such as
    i128/u128 token amounts) are preserved as exact decimal strings or Decimals.
    """

    contract_id: str
    event_name: str
    ledger: Int64Value
    topics: list[JsonValue]
    data: JsonValue
    value: JsonValue


`)
}

func collectDirectSchemaDeps(s Schema, deps map[string]bool) {
	if s.Ref != "" {
		deps[strings.TrimPrefix(s.Ref, "#/components/schemas/")] = true
	}
	for _, prop := range s.Properties {
		collectDirectSchemaDeps(prop, deps)
	}
	if s.Items != nil {
		collectDirectSchemaDeps(*s.Items, deps)
	}
	if s.AdditionalProperties != nil {
		collectDirectSchemaDeps(*s.AdditionalProperties, deps)
	}
}

func sortSchemasTopologically(schemas map[string]Schema) []string {
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)

	visited := map[string]bool{}
	ordered := make([]string, 0, len(names))

	var visit func(name string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		depsMap := map[string]bool{}
		if s, ok := schemas[name]; ok {
			collectDirectSchemaDeps(s, depsMap)
		}
		depNames := make([]string, 0, len(depsMap))
		for dep := range depsMap {
			if dep != name {
				depNames = append(depNames, dep)
			}
		}
		sort.Strings(depNames)
		for _, dep := range depNames {
			if _, exists := schemas[dep]; exists {
				visit(dep)
			}
		}
		ordered = append(ordered, name)
	}

	for _, name := range names {
		visit(name)
	}
	return ordered
}

func writeComponentSchemas(b *bytes.Buffer, doc *OpenAPIDoc) error {
	names := sortSchemasTopologically(doc.Components.Schemas)

	// Emit named Literal union types for schemas with enum properties first.
	if errSchema, ok := doc.Components.Schemas["Error"]; ok {
		if codeProp, ok := errSchema.Properties["code"]; ok && len(codeProp.Enum) > 0 {
			b.WriteString("# Stable machine-readable error codes returned in the API Error envelope.\n")
			fmt.Fprintf(b, "ErrorCode: TypeAlias = Literal[\n    %s,\n]\n\n", joinQuoted(codeProp.Enum, ",\n    "))
		}
	}
	if ruleSchema, ok := doc.Components.Schemas["Rule"]; ok {
		if typeProp, ok := ruleSchema.Properties["type"]; ok && len(typeProp.Enum) > 0 {
			b.WriteString("# Built-in rule evaluation types.\n")
			fmt.Fprintf(b, "RuleTypeName: TypeAlias = Literal[\n    %s,\n]\n\n", joinQuoted(typeProp.Enum, ",\n    "))
		}
	}
	if chSchema, ok := doc.Components.Schemas["ChannelInput"]; ok {
		if typeProp, ok := chSchema.Properties["type"]; ok && len(typeProp.Enum) > 0 {
			b.WriteString("# Supported notification delivery channel types.\n")
			fmt.Fprintf(b, "ChannelType: TypeAlias = Literal[\n    %s,\n]\n\n", joinQuoted(typeProp.Enum, ",\n    "))
		}
	}

	for _, name := range names {
		schema := doc.Components.Schemas[name]
		fmt.Fprintf(b, "class %s(TypedDict, total=False):\n", name)
		writeClassDocstring(b, schema.Description, name)

		reqSet := map[string]bool{}
		for _, r := range schema.Required {
			reqSet[r] = true
		}

		alwaysPresentReadFields := readModelGuaranteedFields(name)

		propNames := make([]string, 0, len(schema.Properties))
		for propName := range schema.Properties {
			propNames = append(propNames, propName)
		}
		sort.Strings(propNames)

		if len(propNames) == 0 {
			b.WriteString("    pass\n\n\n")
			continue
		}

		for _, propName := range propNames {
			prop := schema.Properties[propName]
			if prop.Description != "" {
				writeFieldComment(b, prop.Description, "    ")
			}
			isReq := reqSet[propName] || alwaysPresentReadFields[propName]
			pyType := schemaPropertyType(name, propName, prop, isInputSchema(name))
			if strings.Contains(strings.ToLower(prop.Description), "null when") ||
				(name == "APIToken" || name == "CreatedAPIToken") && (propName == "expires_at" || propName == "last_used_at" || propName == "revoked_at" || propName == "rotated_at") {
				pyType += " | None"
			}
			if isReq {
				fmt.Fprintf(b, "    %s: Required[%s]\n", propName, pyType)
			} else {
				fmt.Fprintf(b, "    %s: NotRequired[%s]\n", propName, pyType)
			}
		}

		b.WriteString("\n\n")
	}
	return nil
}

func isInputSchema(schemaName string) bool {
	return strings.HasSuffix(schemaName, "Input")
}

func readModelGuaranteedFields(schemaName string) map[string]bool {
	switch schemaName {
	case "Monitor":
		return map[string]bool{
			"id": true, "name": true, "contract_ids": true, "enabled": true,
		}
	case "Rule":
		return map[string]bool{
			"id": true, "monitor_id": true, "type": true, "params": true, "enabled": true,
		}
	case "Channel":
		return map[string]bool{
			"id": true, "name": true, "type": true, "enabled": true,
		}
	case "Alert":
		return map[string]bool{
			"id": true, "monitor_id": true, "rule_id": true, "event_id": true,
			"event_name": true, "payload": true, "created_at": true,
		}
	case "MonitorPage":
		return map[string]bool{"monitors": true}
	case "ChannelPage":
		return map[string]bool{"channels": true}
	case "AlertPage":
		return map[string]bool{"alerts": true}
	case "MaintenanceWindow":
		return map[string]bool{
			"id": true, "reason": true, "scope": true, "start_at": true, "end_at": true, "created_at": true,
		}
	case "MaintenanceWindowPage":
		return map[string]bool{"maintenance_windows": true}
	case "Inhibition":
		return map[string]bool{
			"source_rule_id": true, "target_rule_id": true, "firing_window_seconds": true, "created_at": true,
		}
	case "DeliveryAttempt":
		return map[string]bool{
			"alert_id": true, "channel_id": true, "status": true, "attempted_at": true,
		}
	case "EscalationPolicy":
		return map[string]bool{
			"id": true, "monitor_id": true, "steps": true, "created_at": true, "updated_at": true,
		}
	case "EscalationStep":
		return map[string]bool{
			"position": true, "delay": true, "channel_ids": true,
		}
	case "Version":
		return map[string]bool{
			"version": true, "commit": true, "build_date": true,
		}
	case "MonitorStats":
		return map[string]bool{
			"monitor_id": true, "alerts": true, "alerts_last_24h": true, "alerts_last_7d": true,
			"deliveries_succeeded": true, "deliveries_failed": true, "rules": true,
		}
	case "ChannelDeliveryStats":
		return map[string]bool{
			"channel_id": true, "window": true, "total": true, "succeeded": true, "failed": true,
		}
	case "MonitorTemplate":
		return map[string]bool{
			"id": true, "name": true, "created_at": true,
		}
	default:
		return nil
	}
}

func schemaPropertyType(schemaName, propName string, s Schema, isInput bool) string {
	if schemaName == "Error" && propName == "code" {
		return "ErrorCode"
	}
	if (schemaName == "Rule" || schemaName == "RuleInput") && propName == "type" {
		return "RuleTypeName"
	}
	if (schemaName == "Channel" || schemaName == "ChannelInput") && propName == "type" {
		return "ChannelType"
	}
	if schemaName == "Alert" && propName == "payload" {
		return "AlertPayload"
	}
	return schemaToPythonType(s, isInput)
}

func schemaToPythonType(s Schema, isInput bool) string {
	if s.Ref != "" {
		return strings.TrimPrefix(s.Ref, "#/components/schemas/")
	}
	if len(s.Enum) > 0 {
		return "Literal[" + joinQuoted(s.Enum, ", ") + "]"
	}
	switch s.Type {
	case "string":
		switch s.Format {
		case "date-time":
			return "ISODateTimeString"
		case "date":
			return "ISODateString"
		default:
			return "str"
		}
	case "integer":
		if s.Format == "int64" {
			if isInput {
				return "Int64"
			}
			return "Int64Value"
		}
		return "int"
	case "number":
		return "Decimal"
	case "boolean":
		return "bool"
	case "array":
		if s.Items == nil {
			return "list[JsonValue]"
		}
		return "list[" + schemaToPythonType(*s.Items, isInput) + "]"
	case "object":
		if s.AdditionalProperties != nil {
			return "dict[str, " + schemaToPythonType(*s.AdditionalProperties, isInput) + "]"
		}
		return "JsonObject"
	default:
		return "JsonValue"
	}
}

func writeSupplementaryEndpointTypes(b *bytes.Buffer) {
	b.WriteString(`class MonitorUpdateInput(TypedDict, total=False):
    """Partial update payload for PATCH /monitors/{id}."""

    name: str
    contract_ids: list[str]
    enabled: bool
    network: str
    tags: list[str]
    channel_ids: list[Int64]


class RuleUpdateInput(TypedDict, total=False):
    """Partial update payload for PATCH /monitors/{id}/rules/{ruleID}."""

    type: RuleTypeName
    params: JsonObject
    enabled: bool


class ChannelUpdateInput(TypedDict, total=False):
    """Partial update payload for PATCH /channels/{id}.

    "config" is accepted on write when rotating or updating channel
    credentials, and is never echoed back in Channel read responses.
    """

    name: str
    type: ChannelType
    config: JsonObject
    enabled: bool
    digest_mode: bool
    digest_window_seconds: Int64
    timeout: int


class MaintenanceWindowUpdateInput(TypedDict, total=False):
    """Partial update payload for PATCH /maintenance-windows/{id}."""

    reason: str
    scope: Literal["global", "monitor", "contract"]
    monitor_id: Int64
    contract_id: str
    start_at: ISODateTimeString
    end_at: ISODateTimeString


class MonitorTemplateUpdateInput(TypedDict, total=False):
    """Partial update payload for PATCH /templates/{id}."""

    name: str
    description: str
    rules: list[RuleInput]
    channel_ids: list[Int64]
    parameters: list[str]


class ListMonitorsParams(TypedDict, total=False):
    """Query parameters for GET /monitors and GET /monitors/trash."""

    q: str
    enabled: bool
    tag: str
    network: str
    sort: Literal["id_asc", "id_desc", "name_asc", "name_desc"]
    limit: int
    cursor: str


class ListChannelsParams(TypedDict, total=False):
    """Query parameters for GET /channels."""

    q: str
    type: ChannelType
    enabled: bool
    sort: Literal["id_asc", "id_desc", "name_asc", "name_desc"]
    limit: int
    cursor: str


ListAlertsParams = TypedDict(
    "ListAlertsParams",
    {
        "monitor_id": Int64,
        "rule_id": Int64,
        "contract_id": str,
        "q": str,
        "sort": Literal["created_at_desc", "created_at_asc"],
        "from": ISODateTimeString,
        "to": ISODateTimeString,
        "network": str,
        "limit": int,
        "cursor": str,
    },
    total=False,
)


class ListMaintenanceWindowsParams(TypedDict, total=False):
    """Query parameters for GET /maintenance-windows."""

    active: bool
    upcoming: bool
    limit: int


ListAuditParams = TypedDict(
    "ListAuditParams",
    {
        "target_type": Literal["monitor", "rule", "channel", "alert"],
        "target_id": Int64,
        "actor": str,
        "cursor": Int64,
        "from": ISODateTimeString,
        "to": ISODateTimeString,
        "limit": int,
    },
    total=False,
)


class AuditEntry(TypedDict, total=False):
    """Single entry in the append-only configuration audit log (GET /audit)."""

    id: Required[Int64Value]
    actor: Required[str]
    action: Required[str]
    target_type: Required[str]
    target_id: Required[Int64Value]
    target_name: NotRequired[str]
    diff: NotRequired[JsonObject]
    created_at: Required[ISODateTimeString]


class AuditPage(TypedDict, total=False):
    """Response envelope for GET /audit."""

    entries: Required[list[AuditEntry]]
    next_cursor: NotRequired[Int64Value]


class DailyAlertCountDay(TypedDict):
    """Daily alert count bucket within DailyAlertCountsResponse."""

    day: ISODateString
    count: Int64Value


class DailyAlertCountsResponse(TypedDict):
    """Response envelope for GET /stats/alerts-daily."""

    timezone: str
    days: list[DailyAlertCountDay]


class RetentionGap(TypedDict):
    """Detected RPC ledger retention gap reported in ServiceStats."""

    network: str
    from_ledger: Int64Value
    to_ledger: Int64Value
    detected_at: ISODateTimeString


class ServiceStats(TypedDict, total=False):
    """Service statistics returned by GET /stats."""

    monitors: Int64Value
    rules: Int64Value
    channels: Int64Value
    alerts: Int64Value
    deliveries_succeeded: Int64Value
    deliveries_failed: Int64Value
    last_processed_ledger: Int64Value
    retention_gap: RetentionGap


class FederatedIngestInput(TypedDict, total=False):
    """Request payload for POST /ingest (federated alert ingestion)."""

    alert: Required[Alert]
    origin: Required[str]
    hop_count: NotRequired[int]


class FederatedIngestResult(TypedDict, total=False):
    """Response payload for POST /ingest."""

    outcome: Required[Literal["created", "duplicate", "suppressed"]]
    alert_id: NotRequired[Int64Value]
    origin: NotRequired[str]


class TriggerEvaluationInput(TypedDict, total=False):
    """Request payload for POST /triggers."""

    monitor_id: Required[Int64]
    event: NotRequired[str]
    source: NotRequired[str]


class TriggerEvaluationResult(TypedDict, total=False):
    """Response payload for POST /triggers."""

    alert_id: Int64Value
    note: str


class RotateTokenInput(TypedDict, total=False):
    """Optional rotation parameters for POST /tokens/{id}/rotate."""

    name: str
    scopes: list[str]
    overlap: str
    expires_in: str


class ChannelSigningKey(TypedDict, total=False):
    """Channel webhook signing key metadata (secret is omitted on read)."""

    id: str
    key_id: str
    status: str
    created_at: ISODateTimeString
    retired_at: ISODateTimeString | None
    revoked_at: ISODateTimeString | None


class RotatedChannelSigningKey(TypedDict, total=False):
    """Created or rotated channel signing key response (secret returned once)."""

    id: str
    key_id: str
    secret: str
    created_at: ISODateTimeString


class PersistedGraphQLOperation(TypedDict, total=False):
    """Persisted GraphQL operation entry."""

    id: str
    name: str
    query: str
    created_at: ISODateTimeString


`)
}

func writeWideIntAndErrorRuntime(b *bytes.Buffer) {
	b.WriteString(`MAX_SAFE_INT = 9007199254740991
MIN_SAFE_INT = -9007199254740991


def to_decimal(value: Int64) -> Decimal:
    """Convert an Int64 (Decimal, str, or int) to an exact Decimal.

    Rejects float and bool values so callers cannot accidentally pass a
    precision-corrupted IEEE-754 float.
    """
    if isinstance(value, bool) or isinstance(value, float):
        raise TypeError(
            f"float/bool values cannot represent wide integers losslessly: {value!r}"
        )
    if isinstance(value, Decimal):
        return value
    if isinstance(value, int):
        return Decimal(value)
    if isinstance(value, str):
        stripped = value.strip()
        if not stripped:
            raise ValueError("empty integer string")
        try:
            return Decimal(stripped)
        except InvalidOperation as exc:
            raise ValueError(f"invalid decimal integer string: {value!r}") from exc
    raise TypeError(f"unsupported Int64 value type: {type(value).__name__}")


def to_int(value: Int64) -> int:
    """Convert an Int64 (Decimal, str, or int) to an exact Python int.

    Rejects float and bool values so wide 64-bit and 128-bit integers never
    pass through IEEE-754 binary64 rounding.
    """
    if isinstance(value, bool) or isinstance(value, float):
        raise TypeError(
            f"float/bool values cannot represent wide integers losslessly: {value!r}"
        )
    if isinstance(value, int):
        return value
    if isinstance(value, Decimal):
        integral = value.to_integral_value()
        if value != integral:
            raise ValueError(f"non-integral Decimal cannot convert to int: {value!r}")
        return int(integral)
    if isinstance(value, str):
        stripped = value.strip()
        if not stripped:
            raise ValueError("empty integer string")
        return int(stripped, 10)
    raise TypeError(f"unsupported Int64 value type: {type(value).__name__}")


def _parse_json_int(token: str) -> int | str:
    val = int(token, 10)
    if val > MAX_SAFE_INT or val < MIN_SAFE_INT:
        return token
    return val


def parse_json_preserving_wide_ints(raw: str) -> Any:
    """Parse a JSON payload while preserving wide integers and exact decimals.

    Integer tokens outside [-9007199254740991, 9007199254740991] are returned
    as exact base-10 strings (str), and fractional number tokens are parsed
    as Decimal rather than float.
    """
    return json.loads(raw, parse_int=_parse_json_int, parse_float=Decimal)


_DECIMAL_SENTINEL_PREFIX = "__SOROBEACON_NUM__:"
_DECIMAL_SENTINEL_RE = re.compile(r'"__SOROBEACON_NUM__:(-?\d+(?:\.\d+)?)"')


def _prepare_for_json(value: Any) -> Any:
    if isinstance(value, bool) or value is None or isinstance(value, str):
        return value
    if isinstance(value, float):
        raise TypeError(
            f"float values are not permitted in SoroBeacon payloads ({value!r}); "
            "use Decimal, int, or str instead"
        )
    if isinstance(value, int):
        return _DECIMAL_SENTINEL_PREFIX + str(value)
    if isinstance(value, Decimal):
        if not value.is_finite():
            raise ValueError(f"non-finite Decimal is not valid JSON: {value!r}")
        integral = value.to_integral_value()
        if value == integral:
            return _DECIMAL_SENTINEL_PREFIX + str(int(integral))
        return _DECIMAL_SENTINEL_PREFIX + format(value, "f")
    if isinstance(value, Mapping):
        return {str(k): _prepare_for_json(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_prepare_for_json(v) for v in value]
    return value


def stringify_json_with_wide_ints(value: Any) -> str:
    """Serialize a payload to JSON, emitting wide ints and Decimals losslessly.

    Rejects float values so 64-bit IDs, ledger sequences, and stroop amounts
    can never be silently rounded before reaching the wire.
    """
    prepared = _prepare_for_json(value)
    encoded = json.dumps(prepared, separators=(",", ":"), ensure_ascii=False)
    return _DECIMAL_SENTINEL_RE.sub(r"\1", encoded)


class SoroBeaconError(Exception):
    """Base exception for all SoroBeacon Python client errors."""


class SoroBeaconAPIError(SoroBeaconError):
    """Typed exception raised when the SoroBeacon API returns a non-2xx status.

    Exposes status, machine-readable code from the Error taxonomy,
    correlation request_id, and field-level validation details so callers
    can branch on structured properties without string matching.
    """

    status: int
    error: str
    code: ErrorCode | str | None
    request_id: str | None
    details: list[ErrorDetail] | None

    def __init__(
        self,
        status: int,
        envelope: Mapping[str, Any],
    ) -> None:
        err_msg = str(envelope.get("error") or f"HTTP {status}")
        super().__init__(err_msg)
        self.status = status
        self.error = err_msg
        raw_code = envelope.get("code")
        self.code = str(raw_code) if isinstance(raw_code, str) and raw_code else None
        raw_req_id = envelope.get("request_id")
        self.request_id = (
            str(raw_req_id) if isinstance(raw_req_id, str) and raw_req_id else None
        )
        raw_details = envelope.get("details")
        self.details = list(raw_details) if isinstance(raw_details, list) else None

    @staticmethod
    def is_error(err: BaseException, code: ErrorCode | str | None = None) -> bool:
        """Return True when err is a SoroBeaconAPIError (optionally matching code)."""
        if not isinstance(err, SoroBeaconAPIError):
            return False
        return code is None or err.code == code

    def is_not_found(self) -> bool:
        return self.code == "NOT_FOUND" or self.status == 404

    def is_already_exists(self) -> bool:
        return self.code == "ALREADY_EXISTS"

    def is_invalid_argument(self) -> bool:
        return self.code == "INVALID_ARGUMENT" or self.status in (400, 422)

    def is_conflict(self) -> bool:
        return self.code == "CONFLICT" or self.status == 409

    def is_unauthenticated(self) -> bool:
        return self.code == "UNAUTHENTICATED" or self.status == 401

    def is_permission_denied(self) -> bool:
        return self.code == "PERMISSION_DENIED" or self.status == 403

    def is_rate_limited(self) -> bool:
        return self.code == "RATE_LIMITED" or self.status == 429

    def is_quota_exceeded(self) -> bool:
        return self.code == "QUOTA_EXCEEDED"

    def is_payload_too_large(self) -> bool:
        return self.code == "PAYLOAD_TOO_LARGE" or self.status == 413

    def is_service_unavailable(self) -> bool:
        return self.code == "SERVICE_UNAVAILABLE" or self.status == 503


`)
}

func writeRouteTable(b *bytes.Buffer, doc *OpenAPIDoc) {
	b.WriteString("# Complete table of OpenAPI operations covered by SoroBeaconClient.\n")
	b.WriteString("OPENAPI_OPERATIONS: tuple[dict[str, str], ...] = (\n")
	for _, rb := range knownRouteBindings {
		summary := ""
		if item, ok := doc.Paths[rb.Path]; ok {
			if op := operationsMap(item)[rb.Method]; op != nil {
				summary = op.Summary
			}
		}
		fmt.Fprintf(
			b,
			"    {\"method\": %q, \"path\": %q, \"client_method\": %q, \"summary\": %q},\n",
			rb.Method,
			rb.Path,
			rb.ClientMethod,
			summary,
		)
	}
	b.WriteString(")\n\n\n")
}

func writeClientClass(b *bytes.Buffer) {
	b.WriteString(`class SoroBeaconClient:
    """Generated Python client for the SoroBeacon HTTP API (/api/v1)."""

    def __init__(
        self,
        base_url: str,
        token: str | None = None,
        *,
        timeout: float = 30.0,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        trimmed = base_url.rstrip("/")
        self._base_url = (
            trimmed if trimmed.endswith("/api/v1") else f"{trimmed}/api/v1"
        )
        self._token = token
        self._timeout = timeout
        self._default_headers: dict[str, str] = dict(headers) if headers else {}

    def _build_url(
        self,
        path: str,
        query: Mapping[str, Any] | None = None,
    ) -> str:
        url = self._base_url + path
        if not query:
            return url
        pairs: list[tuple[str, str]] = []
        for k, v in query.items():
            if v is None:
                continue
            if isinstance(v, bool):
                pairs.append((k, "true" if v else "false"))
            elif isinstance(v, float):
                raise TypeError(
                    f"float query parameter {k}={v!r} is not permitted; "
                    "use Decimal, int, or str"
                )
            else:
                pairs.append((k, str(v)))
        if not pairs:
            return url
        return f"{url}?{urllib.parse.urlencode(pairs)}"

    def _build_headers(
        self,
        extra: Mapping[str, str] | None = None,
        has_json_body: bool = False,
    ) -> dict[str, str]:
        headers: dict[str, str] = {
            "Accept": "application/json",
            **self._default_headers,
        }
        if extra:
            headers.update(extra)
        if has_json_body and "Content-Type" not in headers:
            headers["Content-Type"] = "application/json"
        if self._token and "Authorization" not in headers:
            headers["Authorization"] = f"Bearer {self._token}"
        return headers

    def _request(
        self,
        method: str,
        path: str,
        *,
        query: Mapping[str, Any] | None = None,
        body: Any = None,
        has_body: bool = False,
        raw_body: str | None = None,
        content_type: str | None = None,
        accept: str | None = None,
        parse_as_text: bool = False,
        headers: Mapping[str, str] | None = None,
    ) -> Any:
        req_headers = self._build_headers(headers, has_json_body=has_body)
        if content_type:
            req_headers["Content-Type"] = content_type
        if accept:
            req_headers["Accept"] = accept

        data_bytes: bytes | None = None
        if has_body:
            data_bytes = stringify_json_with_wide_ints(body).encode("utf-8")
        elif raw_body is not None:
            data_bytes = raw_body.encode("utf-8")

        req = urllib.request.Request(
            self._build_url(path, query),
            data=data_bytes,
            headers=req_headers,
            method=method,
        )

        try:
            with urllib.request.urlopen(req, timeout=self._timeout) as resp:
                status = resp.status
                raw_text = resp.read().decode("utf-8")
                if status == 204 or not raw_text:
                    return None
                if parse_as_text:
                    return raw_text
                return parse_json_preserving_wide_ints(raw_text)
        except urllib.error.HTTPError as err:
            status = err.code
            raw_text = ""
            if err.fp is not None:
                try:
                    raw_text = err.read().decode("utf-8", errors="replace")
                except Exception:
                    raw_text = ""
            hdr_req_id = err.headers.get("X-Request-ID") if err.headers else None
            envelope: dict[str, Any] = {
                "error": raw_text or err.reason or f"HTTP {status}",
            }
            if raw_text:
                try:
                    parsed = parse_json_preserving_wide_ints(raw_text)
                    if isinstance(parsed, dict) and isinstance(parsed.get("error"), str):
                        envelope = {
                            "error": parsed["error"],
                            "code": parsed.get("code"),
                            "request_id": parsed.get("request_id") or hdr_req_id,
                            "details": parsed.get("details"),
                        }
                except Exception:
                    pass
            if not envelope.get("request_id") and hdr_req_id:
                envelope["request_id"] = hdr_req_id
            raise SoroBeaconAPIError(status, envelope) from None

    @staticmethod
    def _quote(seg: Int64 | str) -> str:
        if isinstance(seg, bool) or isinstance(seg, float):
            raise TypeError(f"invalid path parameter: {seg!r}")
        return urllib.parse.quote(str(seg), safe="")

    # -------------------------------------------------------------------------
    # Monitors
    # -------------------------------------------------------------------------

    def list_monitors(
        self,
        params: ListMonitorsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorPage:
        return self._request("GET", "/monitors", query=params, headers=headers)

    def paginate_monitors(
        self,
        params: ListMonitorsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Iterator[Monitor]:
        """Iterate transparently across all GET /monitors pages via next_cursor."""
        base_query: dict[str, Any] = dict(params) if params else {}
        cursor: str | None = None
        while True:
            if cursor is not None:
                base_query["cursor"] = cursor
            else:
                base_query.pop("cursor", None)
            page = self.list_monitors(base_query, headers=headers)
            for item in page.get("monitors") or []:
                yield item
            next_cursor = page.get("next_cursor")
            if not next_cursor:
                break
            cursor = str(next_cursor)

    def create_monitor(
        self,
        input_data: MonitorInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        return self._request(
            "POST", "/monitors", body=input_data, has_body=True, headers=headers
        )

    def get_monitor(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        return self._request(
            "GET", f"/monitors/{self._quote(monitor_id)}", headers=headers
        )

    def update_monitor(
        self,
        monitor_id: Int64,
        input_data: MonitorUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        return self._request(
            "PATCH",
            f"/monitors/{self._quote(monitor_id)}",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def delete_monitor(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "DELETE", f"/monitors/{self._quote(monitor_id)}", headers=headers
        )

    def bulk_update_monitors(
        self,
        input_data: BulkMonitorInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> BulkMonitorResult:
        return self._request(
            "POST", "/monitors/bulk", body=input_data, has_body=True, headers=headers
        )

    def import_monitors(
        self,
        input_data: ImportInput | str,
        *,
        query: Mapping[str, Any] | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        if isinstance(input_data, str):
            return self._request(
                "POST",
                "/monitors/import",
                query=query,
                raw_body=input_data,
                content_type="text/csv",
                headers=headers,
            )
        return self._request(
            "POST",
            "/monitors/import",
            query=query,
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def list_trash_monitors(
        self,
        params: ListMonitorsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorPage:
        return self._request("GET", "/monitors/trash", query=params, headers=headers)

    def paginate_trash_monitors(
        self,
        params: ListMonitorsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Iterator[Monitor]:
        """Iterate transparently across GET /monitors/trash pages via next_cursor."""
        base_query: dict[str, Any] = dict(params) if params else {}
        cursor: str | None = None
        while True:
            if cursor is not None:
                base_query["cursor"] = cursor
            else:
                base_query.pop("cursor", None)
            page = self.list_trash_monitors(base_query, headers=headers)
            for item in page.get("monitors") or []:
                yield item
            next_cursor = page.get("next_cursor")
            if not next_cursor:
                break
            cursor = str(next_cursor)

    def soft_delete_monitor(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "POST",
            f"/monitors/{self._quote(monitor_id)}/soft-delete",
            headers=headers,
        )

    def restore_monitor(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        return self._request(
            "POST",
            f"/monitors/{self._quote(monitor_id)}/restore",
            headers=headers,
        )

    def duplicate_monitor(
        self,
        monitor_id: Int64,
        input_data: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        return self._request(
            "POST",
            f"/monitors/{self._quote(monitor_id)}/duplicate",
            body=dict(input_data) if input_data else {},
            has_body=True,
            headers=headers,
        )

    def get_monitor_stats(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorStats:
        return self._request(
            "GET", f"/monitors/{self._quote(monitor_id)}/stats", headers=headers
        )

    # -------------------------------------------------------------------------
    # Escalation Policies & Rules
    # -------------------------------------------------------------------------

    def get_monitor_escalation(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> EscalationPolicy:
        return self._request(
            "GET",
            f"/monitors/{self._quote(monitor_id)}/escalation",
            headers=headers,
        )

    def set_monitor_escalation(
        self,
        monitor_id: Int64,
        input_data: EscalationInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> EscalationPolicy:
        return self._request(
            "PUT",
            f"/monitors/{self._quote(monitor_id)}/escalation",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def delete_monitor_escalation(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "DELETE",
            f"/monitors/{self._quote(monitor_id)}/escalation",
            headers=headers,
        )

    def list_rules(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[Rule]:
        return self._request(
            "GET", f"/monitors/{self._quote(monitor_id)}/rules", headers=headers
        )

    def create_rule(
        self,
        monitor_id: Int64,
        input_data: RuleInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Rule:
        return self._request(
            "POST",
            f"/monitors/{self._quote(monitor_id)}/rules",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def bulk_create_rules(
        self,
        monitor_id: Int64,
        rules: Sequence[RuleInput],
        *,
        headers: Mapping[str, str] | None = None,
    ) -> dict[str, list[Rule]]:
        return self._request(
            "POST",
            f"/monitors/{self._quote(monitor_id)}/rules/bulk",
            body={"rules": list(rules)},
            has_body=True,
            headers=headers,
        )

    def dry_run_rule(
        self,
        monitor_id: Int64,
        input_data: RuleInput,
        *,
        query: Mapping[str, Any] | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> DryRunResult:
        return self._request(
            "POST",
            f"/monitors/{self._quote(monitor_id)}/rules/dry-run",
            query=query,
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def update_rule(
        self,
        monitor_id: Int64,
        rule_id: Int64,
        input_data: RuleUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Rule:
        return self._request(
            "PATCH",
            f"/monitors/{self._quote(monitor_id)}/rules/{self._quote(rule_id)}",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def delete_rule(
        self,
        monitor_id: Int64,
        rule_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "DELETE",
            f"/monitors/{self._quote(monitor_id)}/rules/{self._quote(rule_id)}",
            headers=headers,
        )

    # -------------------------------------------------------------------------
    # Channels & Channel Signing Keys
    # -------------------------------------------------------------------------

    def list_channels(
        self,
        params: ListChannelsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> ChannelPage:
        return self._request("GET", "/channels", query=params, headers=headers)

    def paginate_channels(
        self,
        params: ListChannelsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Iterator[Channel]:
        """Iterate transparently across GET /channels pages via next_cursor."""
        base_query: dict[str, Any] = dict(params) if params else {}
        cursor: str | None = None
        while True:
            if cursor is not None:
                base_query["cursor"] = cursor
            else:
                base_query.pop("cursor", None)
            page = self.list_channels(base_query, headers=headers)
            for item in page.get("channels") or []:
                yield item
            next_cursor = page.get("next_cursor")
            if not next_cursor:
                break
            cursor = str(next_cursor)

    def create_channel(
        self,
        input_data: ChannelInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Channel:
        return self._request(
            "POST", "/channels", body=input_data, has_body=True, headers=headers
        )

    def get_channel(
        self,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Channel:
        return self._request(
            "GET", f"/channels/{self._quote(channel_id)}", headers=headers
        )

    def update_channel(
        self,
        channel_id: Int64,
        input_data: ChannelUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Channel:
        return self._request(
            "PATCH",
            f"/channels/{self._quote(channel_id)}",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def delete_channel(
        self,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "DELETE", f"/channels/{self._quote(channel_id)}", headers=headers
        )

    def bulk_update_channels(
        self,
        input_data: Mapping[str, Any],
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "POST", "/channels/bulk", body=input_data, has_body=True, headers=headers
        )

    def get_channel_stats(
        self,
        channel_id: Int64,
        *,
        query: Mapping[str, Any] | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> ChannelDeliveryStats:
        return self._request(
            "GET",
            f"/channels/{self._quote(channel_id)}/stats",
            query=query,
            headers=headers,
        )

    def duplicate_channel(
        self,
        channel_id: Int64,
        input_data: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Channel:
        return self._request(
            "POST",
            f"/channels/{self._quote(channel_id)}/duplicate",
            body=dict(input_data) if input_data else {},
            has_body=True,
            headers=headers,
        )

    def test_channel(
        self,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "POST", f"/channels/{self._quote(channel_id)}/test", headers=headers
        )

    def list_channel_keys(
        self,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Any:
        return self._request(
            "GET", f"/channels/{self._quote(channel_id)}/keys", headers=headers
        )

    def rotate_channel_key(
        self,
        channel_id: Int64,
        input_data: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> RotatedChannelSigningKey:
        return self._request(
            "POST",
            f"/channels/{self._quote(channel_id)}/keys/rotate",
            body=dict(input_data) if input_data else {},
            has_body=True,
            headers=headers,
        )

    def retire_channel_key(
        self,
        channel_id: Int64,
        key_id: str,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "POST",
            f"/channels/{self._quote(channel_id)}/keys/{self._quote(key_id)}/retire",
            headers=headers,
        )

    def revoke_channel_key(
        self,
        channel_id: Int64,
        key_id: str,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "POST",
            f"/channels/{self._quote(channel_id)}/keys/{self._quote(key_id)}/revoke",
            headers=headers,
        )

    # -------------------------------------------------------------------------
    # Maintenance Windows & Inhibitions
    # -------------------------------------------------------------------------

    def list_maintenance_windows(
        self,
        params: ListMaintenanceWindowsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MaintenanceWindowPage:
        return self._request(
            "GET", "/maintenance-windows", query=params, headers=headers
        )

    def create_maintenance_window(
        self,
        input_data: MaintenanceWindowInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MaintenanceWindow:
        return self._request(
            "POST",
            "/maintenance-windows",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def get_maintenance_window(
        self,
        window_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MaintenanceWindow:
        return self._request(
            "GET",
            f"/maintenance-windows/{self._quote(window_id)}",
            headers=headers,
        )

    def update_maintenance_window(
        self,
        window_id: Int64,
        input_data: MaintenanceWindowUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MaintenanceWindow:
        return self._request(
            "PATCH",
            f"/maintenance-windows/{self._quote(window_id)}",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def delete_maintenance_window(
        self,
        window_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "DELETE",
            f"/maintenance-windows/{self._quote(window_id)}",
            headers=headers,
        )

    def list_inhibitions(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> dict[str, list[Inhibition]]:
        return self._request("GET", "/inhibitions", headers=headers)

    def create_inhibition(
        self,
        input_data: InhibitionInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Inhibition:
        return self._request(
            "POST", "/inhibitions", body=input_data, has_body=True, headers=headers
        )

    def delete_inhibition(
        self,
        source_id: Int64,
        target_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "DELETE",
            f"/inhibitions/{self._quote(source_id)}/{self._quote(target_id)}",
            headers=headers,
        )

    # -------------------------------------------------------------------------
    # Alerts, Streaming, Exports & Deliveries
    # -------------------------------------------------------------------------

    def list_alerts(
        self,
        params: ListAlertsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> AlertPage:
        return self._request("GET", "/alerts", query=params, headers=headers)

    def paginate_alerts(
        self,
        params: ListAlertsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Iterator[Alert]:
        """Iterate transparently across GET /alerts pages via next_cursor."""
        base_query: dict[str, Any] = dict(params) if params else {}
        cursor: str | None = None
        while True:
            if cursor is not None:
                base_query["cursor"] = cursor
            else:
                base_query.pop("cursor", None)
            page = self.list_alerts(base_query, headers=headers)
            for item in page.get("alerts") or []:
                yield item
            next_cursor = page.get("next_cursor")
            if not next_cursor:
                break
            cursor = str(next_cursor)

    def stream_alerts(
        self,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Iterator[Alert]:
        """Connect to GET /alerts/stream (SSE) and yield decoded Alert objects."""
        req_headers = self._build_headers(headers, has_json_body=False)
        req_headers["Accept"] = "text/event-stream"
        req = urllib.request.Request(
            self._build_url("/alerts/stream", params),
            headers=req_headers,
            method="GET",
        )
        try:
            with urllib.request.urlopen(req, timeout=self._timeout) as resp:
                data_lines: list[str] = []
                for raw_line in resp:
                    line = raw_line.decode("utf-8").rstrip("\r\n")
                    if not line:
                        if data_lines:
                            yield parse_json_preserving_wide_ints("\n".join(data_lines))
                            data_lines = []
                        continue
                    if line.startswith("data:"):
                        data_lines.append(line[5:].lstrip())
                if data_lines:
                    yield parse_json_preserving_wide_ints("\n".join(data_lines))
        except urllib.error.HTTPError as err:
            raw_text = ""
            if err.fp is not None:
                try:
                    raw_text = err.read().decode("utf-8", errors="replace")
                except Exception:
                    raw_text = ""
            raise SoroBeaconAPIError(
                err.code, {"error": raw_text or f"HTTP {err.code}"}
            ) from None

    def export_alerts_csv(
        self,
        params: ListAlertsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> str:
        return self._request(
            "GET",
            "/alerts.csv",
            query=params,
            accept="text/csv",
            parse_as_text=True,
            headers=headers,
        )

    def export_alerts_ndjson(
        self,
        params: ListAlertsParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[Alert]:
        raw = self._request(
            "GET",
            "/alerts/export",
            query=params,
            accept="application/x-ndjson",
            parse_as_text=True,
            headers=headers,
        )
        if not raw or not str(raw).strip():
            return []
        return [
            parse_json_preserving_wide_ints(line)
            for line in str(raw).strip().splitlines()
            if line.strip()
        ]

    def acknowledge_alert(
        self,
        alert_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Alert:
        return self._request(
            "POST",
            f"/alerts/{self._quote(alert_id)}/acknowledge",
            headers=headers,
        )

    def list_alert_deliveries(
        self,
        alert_id: Int64,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[DeliveryAttempt]:
        return self._request(
            "GET",
            f"/alerts/{self._quote(alert_id)}/deliveries",
            query=params,
            headers=headers,
        )

    def retry_alert_delivery(
        self,
        alert_id: Int64,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> DeliveryAttempt:
        return self._request(
            "POST",
            f"/alerts/{self._quote(alert_id)}/deliveries/{self._quote(channel_id)}/retry",
            headers=headers,
        )

    # -------------------------------------------------------------------------
    # Dead Letters, Federated Ingest & External Triggers
    # -------------------------------------------------------------------------

    def list_dead_letters(
        self,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> dict[str, list[JsonObject]]:
        return self._request("GET", "/dead-letters", query=params, headers=headers)

    def list_dead_letter_groups(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[JsonObject]:
        return self._request("GET", "/dead-letters/groups", headers=headers)

    def bulk_redrive_dead_letters(
        self,
        input_data: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "POST",
            "/dead-letters/bulk/redrive",
            body=dict(input_data) if input_data else {},
            has_body=True,
            headers=headers,
        )

    def bulk_discard_dead_letters(
        self,
        input_data: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "POST",
            "/dead-letters/bulk/discard",
            body=dict(input_data) if input_data else {},
            has_body=True,
            headers=headers,
        )

    def redrive_dead_letter(
        self,
        dead_letter_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "POST",
            f"/dead-letters/{self._quote(dead_letter_id)}/redrive",
            headers=headers,
        )

    def discard_dead_letter(
        self,
        dead_letter_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "POST",
            f"/dead-letters/{self._quote(dead_letter_id)}/discard",
            headers=headers,
        )

    def ingest_federated_alert(
        self,
        input_data: FederatedIngestInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> FederatedIngestResult:
        return self._request(
            "POST", "/ingest", body=input_data, has_body=True, headers=headers
        )

    def trigger_evaluation(
        self,
        input_data: TriggerEvaluationInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> TriggerEvaluationResult:
        return self._request(
            "POST", "/triggers", body=input_data, has_body=True, headers=headers
        )

    # -------------------------------------------------------------------------
    # System Health, Poller, Version, Rule Types, Stats & Audit
    # -------------------------------------------------------------------------

    def get_health(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request("GET", "/health", headers=headers)

    def get_livez(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Any:
        return self._request("GET", "/livez", headers=headers)

    def get_readyz(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request("GET", "/readyz", headers=headers)

    def get_poller_status(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> PollerStatus:
        return self._request("GET", "/poller", headers=headers)

    def get_version(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Version:
        return self._request("GET", "/version", headers=headers)

    def list_rule_types(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> RuleTypesResponse:
        return self._request("GET", "/rule-types", headers=headers)

    def get_stats(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> ServiceStats:
        return self._request("GET", "/stats", headers=headers)

    def get_daily_alert_counts(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> DailyAlertCountsResponse:
        return self._request("GET", "/stats/alerts-daily", headers=headers)

    def list_audit_logs(
        self,
        params: ListAuditParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> AuditPage:
        return self._request("GET", "/audit", query=params, headers=headers)

    def paginate_audit_logs(
        self,
        params: ListAuditParams | Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Iterator[AuditEntry]:
        """Iterate transparently across GET /audit pages using the last entry ID."""
        base_query: dict[str, Any] = dict(params) if params else {}
        limit = int(base_query.get("limit") or 50)
        base_query["limit"] = limit
        cursor: Int64 | None = None
        while True:
            if cursor is not None:
                base_query["cursor"] = cursor
            else:
                base_query.pop("cursor", None)
            page = self.list_audit_logs(base_query, headers=headers)
            entries = page.get("entries") or []
            for entry in entries:
                yield entry
            if len(entries) < limit:
                break
            cursor = entries[-1]["id"]

    # -------------------------------------------------------------------------
    # Monitor Templates
    # -------------------------------------------------------------------------

    def list_templates(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> dict[str, list[MonitorTemplate]]:
        return self._request("GET", "/templates", headers=headers)

    def create_template(
        self,
        input_data: MonitorTemplateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorTemplate:
        return self._request(
            "POST", "/templates", body=input_data, has_body=True, headers=headers
        )

    def get_template(
        self,
        template_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorTemplate:
        return self._request(
            "GET", f"/templates/{self._quote(template_id)}", headers=headers
        )

    def update_template(
        self,
        template_id: Int64,
        input_data: MonitorTemplateUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorTemplate:
        return self._request(
            "PATCH",
            f"/templates/{self._quote(template_id)}",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def delete_template(
        self,
        template_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "DELETE", f"/templates/{self._quote(template_id)}", headers=headers
        )

    def instantiate_template(
        self,
        template_id: Int64,
        input_data: InstantiateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        return self._request(
            "POST",
            f"/templates/{self._quote(template_id)}/instantiate",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def bulk_instantiate_template(
        self,
        template_id: Int64,
        instances: Sequence[InstantiateInput],
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "POST",
            f"/templates/{self._quote(template_id)}/instantiate/bulk",
            body={"instances": list(instances)},
            has_body=True,
            headers=headers,
        )

    # -------------------------------------------------------------------------
    # Scoped API Tokens & Persisted GraphQL Operations
    # -------------------------------------------------------------------------

    def list_tokens(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> dict[str, list[APIToken]]:
        return self._request("GET", "/tokens", headers=headers)

    def create_token(
        self,
        input_data: TokenInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> CreatedAPIToken:
        return self._request(
            "POST", "/tokens", body=input_data, has_body=True, headers=headers
        )

    def get_token(
        self,
        token_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> APIToken:
        return self._request(
            "GET", f"/tokens/{self._quote(token_id)}", headers=headers
        )

    def revoke_token(
        self,
        token_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "POST", f"/tokens/{self._quote(token_id)}/revoke", headers=headers
        )

    def rotate_token(
        self,
        token_id: Int64,
        input_data: RotateTokenInput | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> CreatedAPIToken:
        return self._request(
            "POST",
            f"/tokens/{self._quote(token_id)}/rotate",
            body=dict(input_data) if input_data else {},
            has_body=True,
            headers=headers,
        )

    def abandon_token_rotation(
        self,
        token_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> APIToken | None:
        return self._request(
            "POST", f"/tokens/{self._quote(token_id)}/abandon", headers=headers
        )

    def list_graphql_operations(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Any:
        return self._request("GET", "/graphql/operations", headers=headers)

    def register_graphql_operation(
        self,
        input_data: Mapping[str, Any],
        *,
        headers: Mapping[str, str] | None = None,
    ) -> PersistedGraphQLOperation:
        return self._request(
            "POST",
            "/graphql/operations",
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def get_graphql_operation(
        self,
        operation_id: str,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> PersistedGraphQLOperation:
        return self._request(
            "GET",
            f"/graphql/operations/{self._quote(operation_id)}",
            headers=headers,
        )

    def delete_graphql_operation(
        self,
        operation_id: str,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request(
            "DELETE",
            f"/graphql/operations/{self._quote(operation_id)}",
            headers=headers,
        )
`)
}

func joinQuoted(items []string, sep string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, sep)
}

func writeClassDocstring(b *bytes.Buffer, desc, schemaName string) {
	redactionNote := ""
	switch schemaName {
	case "Channel":
		redactionNote = "Redacted on read: channel configuration contains secrets (webhook URLs, bot tokens, SMTP credentials) and is accepted only on ChannelInput."
	case "APIToken":
		redactionNote = "Redacted on read: the plaintext token secret is returned only once in CreatedAPIToken when minting or rotating a token."
	}

	raw := strings.TrimSpace(desc)
	if raw == "" && redactionNote == "" {
		return
	}
	lines := []string{}
	if raw != "" {
		lines = append(lines, strings.Split(raw, "\n")...)
	}
	if redactionNote != "" {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, redactionNote)
	}
	if len(lines) == 1 && len(lines[0]) <= 68 {
		clean := strings.ReplaceAll(strings.TrimSpace(lines[0]), `"""`, `'''`)
		fmt.Fprintf(b, "    \"\"\"%s\"\"\"\n\n", clean)
		return
	}
	b.WriteString("    \"\"\"")
	for i, line := range lines {
		clean := strings.ReplaceAll(strings.TrimSpace(line), `"""`, `'''`)
		switch {
		case i == 0:
			fmt.Fprintf(b, "%s\n", clean)
		case clean == "":
			b.WriteString("\n")
		default:
			fmt.Fprintf(b, "    %s\n", clean)
		}
	}
	b.WriteString("    \"\"\"\n\n")
}

func writeFieldComment(b *bytes.Buffer, desc, indent string) {
	for _, line := range strings.Split(strings.TrimSpace(desc), "\n") {
		clean := strings.TrimSpace(line)
		if clean != "" {
			fmt.Fprintf(b, "%s# %s\n", indent, clean)
		}
	}
}
