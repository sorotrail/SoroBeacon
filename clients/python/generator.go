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
	"strconv"
	"strings"
	"unicode"
)

// OpenAPIDoc represents the subset of OpenAPI 3.1 used by SoroBeacon's
// internal/api/openapi.json specification.
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

// PackageDir returns the directory of this package on disk.
func PackageDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// DefaultSpecPath returns the path to internal/api/openapi.json.
func DefaultSpecPath() string {
	return filepath.Join(PackageDir(), "..", "..", "internal", "api", "openapi.json")
}

// DefaultOutputPath returns the path to the generated Python model module.
func DefaultOutputPath() string {
	return filepath.Join(PackageDir(), "sorobeacon", "_generated.py")
}

// GenerateFromFile reads internal/api/openapi.json and returns the deterministic
// Python source for clients/python/sorobeacon/_generated.py.
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
// route mappings, and emits the generated Python models module.
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
	writeScalarAndJsonHelpers(&b)
	writeEnums(&b, &doc)
	if err := writeComponentSchemas(&b, &doc); err != nil {
		return nil, err
	}
	writeRouteTable(&b, &doc)

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

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation
from enum import Enum
from typing import Final, TypeAlias

SPEC_VERSION: Final[str] = %s

`, doc.Info.Title, doc.Info.Version, strconv.Quote(doc.Info.Version))
}

func writeScalarAndJsonHelpers(b *bytes.Buffer) {
	b.WriteString(`# Wide integers (Soroban i128/u128/i256/u256 and 64-bit resource/ledger IDs)
# are typed as plain strings or exact Decimal instances — never IEEE-754 floats,
# which only have 53 bits of mantissa and silently corrupt values above 2^53 - 1.
WideInt: TypeAlias = str | Decimal
Int64: TypeAlias = int | str | Decimal

ISODateTimeString: TypeAlias = str
ISODateString: TypeAlias = str

JSONScalar: TypeAlias = str | int | Decimal | bool | None
JSONValue: TypeAlias = JSONScalar | list["JSONValue"] | dict[str, "JSONValue"]
JSONObject: TypeAlias = dict[str, JSONValue]


def _is_float_instance(val: object) -> bool:
    return type(val).__name__ == "float"


def _as_str(val: object, default: str = "") -> str:
    return val if isinstance(val, str) else default


def _as_opt_str(val: object) -> str | None:
    return val if isinstance(val, str) else None


def _as_bool(val: object, default: bool = False) -> bool:
    return val if isinstance(val, bool) else default


def _as_opt_bool(val: object) -> bool | None:
    return val if isinstance(val, bool) else None


def _as_int(val: object, default: int = 0) -> int:
    if isinstance(val, bool) or _is_float_instance(val):
        raise TypeError("float and bool are not valid integer values; use int, str, or Decimal")
    if isinstance(val, int):
        return val
    if isinstance(val, Decimal):
        if not val.is_finite() or val != val.to_integral_value():
            raise ValueError(f"non-integral Decimal cannot be converted to int: {val!r}")
        return int(val)
    if isinstance(val, str):
        stripped = val.strip()
        if stripped != "":
            return int(stripped, 10)
    return default


def _as_opt_int(val: object) -> int | None:
    if val is None:
        return None
    if isinstance(val, bool) or _is_float_instance(val):
        raise TypeError("float and bool are not valid integer values; use int, str, or Decimal")
    if isinstance(val, int):
        return val
    if isinstance(val, Decimal):
        if not val.is_finite() or val != val.to_integral_value():
            raise ValueError(f"non-integral Decimal cannot be converted to int: {val!r}")
        return int(val)
    if isinstance(val, str):
        stripped = val.strip()
        if stripped != "":
            return int(stripped, 10)
    return None


def _as_decimal(val: object, default: Decimal = Decimal(0)) -> Decimal:
    if isinstance(val, bool) or _is_float_instance(val):
        raise TypeError("float and bool are not valid Decimal values; use Decimal, int, or str")
    if isinstance(val, Decimal):
        return val
    if isinstance(val, int):
        return Decimal(val)
    if isinstance(val, str):
        stripped = val.strip()
        if stripped != "":
            try:
                return Decimal(stripped)
            except InvalidOperation as exc:
                raise ValueError(f"invalid Decimal string: {val!r}") from exc
    return default


def _as_opt_decimal(val: object) -> Decimal | None:
    if val is None:
        return None
    if isinstance(val, bool) or _is_float_instance(val):
        raise TypeError("float and bool are not valid Decimal values; use Decimal, int, or str")
    if isinstance(val, Decimal):
        return val
    if isinstance(val, int):
        return Decimal(val)
    if isinstance(val, str):
        stripped = val.strip()
        if stripped != "":
            try:
                return Decimal(stripped)
            except InvalidOperation as exc:
                raise ValueError(f"invalid Decimal string: {val!r}") from exc
    return None


def _as_list(val: object) -> list[object]:
    return val if isinstance(val, list) else []


def _as_str_list(val: object) -> list[str]:
    if not isinstance(val, list):
        return []
    return [item for item in val if isinstance(item, str)]


def _as_int_list(val: object) -> list[int]:
    if not isinstance(val, list):
        return []
    return [_as_int(item) for item in val if item is not None]


def _as_json_val(val: object) -> JSONValue:
    if val is None or isinstance(val, (str, bool)):
        return val
    if _is_float_instance(val):
        raise TypeError("float is not permitted in structured JSON payloads; use Decimal, int, or str")
    if isinstance(val, (int, Decimal)):
        return val
    if isinstance(val, list):
        return [_as_json_val(item) for item in val]
    if isinstance(val, Mapping):
        return {str(k): _as_json_val(v) for k, v in val.items()}
    return str(val)


def _as_json_obj(val: object) -> JSONObject:
    if not isinstance(val, Mapping):
        return {}
    return {str(k): _as_json_val(v) for k, v in val.items()}


def _as_opt_json_obj(val: object) -> JSONObject | None:
    if not isinstance(val, Mapping):
        return None
    return {str(k): _as_json_val(v) for k, v in val.items()}


def _as_json_obj_list(val: object) -> list[JSONObject]:
    if not isinstance(val, list):
        return []
    return [_as_json_obj(item) for item in val if isinstance(item, Mapping)]


`)
}

func writeEnums(b *bytes.Buffer, doc *OpenAPIDoc) {
	if errSchema, ok := doc.Components.Schemas["Error"]; ok {
		if codeProp, ok := errSchema.Properties["code"]; ok && len(codeProp.Enum) > 0 {
			b.WriteString("class ErrorCode(str, Enum):\n")
			b.WriteString("    \"\"\"Stable machine-readable error codes returned in the API Error envelope.\"\"\"\n\n")
			for _, v := range codeProp.Enum {
				fmt.Fprintf(b, "    %s = %q\n", enumMemberName(v), v)
			}
			b.WriteString("\n\n")
		}
	}
	if ruleSchema, ok := doc.Components.Schemas["Rule"]; ok {
		if typeProp, ok := ruleSchema.Properties["type"]; ok && len(typeProp.Enum) > 0 {
			b.WriteString("class RuleTypeName(str, Enum):\n")
			b.WriteString("    \"\"\"Built-in rule evaluation types.\"\"\"\n\n")
			for _, v := range typeProp.Enum {
				fmt.Fprintf(b, "    %s = %q\n", enumMemberName(v), v)
			}
			b.WriteString("\n\n")
		}
	}
	if chSchema, ok := doc.Components.Schemas["ChannelInput"]; ok {
		if typeProp, ok := chSchema.Properties["type"]; ok && len(typeProp.Enum) > 0 {
			b.WriteString("class ChannelType(str, Enum):\n")
			b.WriteString("    \"\"\"Supported notification delivery channel types.\"\"\"\n\n")
			for _, v := range typeProp.Enum {
				fmt.Fprintf(b, "    %s = %q\n", enumMemberName(v), v)
			}
			b.WriteString("\n\n")
		}
	}
}

func enumMemberName(val string) string {
	var out strings.Builder
	for _, r := range val {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(unicode.ToUpper(r))
		} else {
			out.WriteRune('_')
		}
	}
	s := out.String()
	if s == "" {
		return "EMPTY"
	}
	return s
}

func schemaOrder(doc *OpenAPIDoc) []string {
	priority := map[string]int{
		"ErrorDetail":       1,
		"Error":             2,
		"EscalationStep":    3,
		"EscalationInput":   4,
		"EscalationPolicy":  5,
		"RuleParameter":     6,
		"RuleType":          7,
		"RuleTypesResponse": 8,
		"RuleMatchCount":    9,
		"MonitorStats":      10,
		"Rule":              11,
		"RuleInput":         12,
		"Monitor":           13,
		"MonitorInput":      14,
		"MonitorPage":       15,
		"Channel":           16,
		"ChannelInput":      17,
		"ChannelPage":       18,
		"Alert":             19,
		"AlertPage":         20,
		"MaintenanceWindow": 21,
		"MaintenanceWindowInput": 22,
		"MaintenanceWindowPage":  23,
		"MonitorTemplate":        24,
		"MonitorTemplateInput":   25,
	}
	names := make([]string, 0, len(doc.Components.Schemas))
	for name := range doc.Components.Schemas {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		pi, oki := priority[names[i]]
		pj, okj := priority[names[j]]
		if oki && okj {
			return pi < pj
		}
		if oki {
			return true
		}
		if okj {
			return false
		}
		return names[i] < names[j]
	})
	return names
}

type pyFieldSpec struct {
	Name       string
	PyType     string
	Default    string
	FromDict   string
	ToDictExpr string
	Optional   bool
	Doc        string
}

func writeComponentSchemas(b *bytes.Buffer, doc *OpenAPIDoc) error {
	for _, name := range schemaOrder(doc) {
		schema := doc.Components.Schemas[name]
		writeDataclassSchema(b, name, schema)
	}
	return nil
}

func writeDataclassSchema(b *bytes.Buffer, name string, schema Schema) {
	reqSet := map[string]bool{}
	for _, r := range schema.Required {
		reqSet[r] = true
	}
	alwaysPresent := readModelGuaranteedFields(name)

	propNames := make([]string, 0, len(schema.Properties))
	for propName := range schema.Properties {
		propNames = append(propNames, propName)
	}
	sort.Strings(propNames)

	var reqFields []pyFieldSpec
	var optFields []pyFieldSpec

	for _, propName := range propNames {
		prop := schema.Properties[propName]
		isReq := reqSet[propName] || alwaysPresent[propName]
		f := buildPyField(name, propName, prop, isReq)
		if f.Optional {
			optFields = append(optFields, f)
		} else {
			reqFields = append(reqFields, f)
		}
	}

	allFields := make([]pyFieldSpec, 0, len(reqFields)+len(optFields))
	allFields = append(allFields, reqFields...)
	allFields = append(allFields, optFields...)

	b.WriteString("@dataclass(slots=True)\n")
	fmt.Fprintf(b, "class %s:\n", name)
	desc := strings.TrimSpace(schema.Description)
	switch name {
	case "Channel":
		desc += " Note: channel configuration (`config`) is write-only on ChannelInput and is never returned by the server."
	case "APIToken":
		desc += " Note: the plaintext token secret (`token`) is returned only once in CreatedAPIToken and is omitted on APIToken."
	}
	if desc != "" {
		fmt.Fprintf(b, "    %s\n\n", strconv.Quote(strings.ReplaceAll(desc, "\n", " ")))
	}

	if len(allFields) == 0 {
		b.WriteString("    pass\n\n\n")
		return
	}

	for _, f := range allFields {
		fmt.Fprintf(b, "    %s: %s = %s\n", f.Name, f.PyType, f.Default)
	}
	b.WriteString("\n")

	// Emit from_dict classmethod.
	fmt.Fprintf(b, "    @classmethod\n")
	fmt.Fprintf(b, "    def from_dict(cls, data: Mapping[str, object]) -> %s:\n", name)
	fmt.Fprintf(b, "        return cls(\n")
	for _, f := range allFields {
		fmt.Fprintf(b, "            %s=%s,\n", f.Name, f.FromDict)
	}
	fmt.Fprintf(b, "        )\n\n")

	// Emit to_dict method.
	fmt.Fprintf(b, "    def to_dict(self) -> JSONObject:\n")
	fmt.Fprintf(b, "        out: JSONObject = {}\n")
	for _, f := range allFields {
		if f.Optional {
			fmt.Fprintf(b, "        if self.%s is not None:\n", f.Name)
			fmt.Fprintf(b, "            out[%q] = %s\n", f.Name, f.ToDictExpr)
		} else {
			fmt.Fprintf(b, "        out[%q] = %s\n", f.Name, f.ToDictExpr)
		}
	}
	fmt.Fprintf(b, "        return out\n\n\n")
}

func buildPyField(schemaName, propName string, s Schema, isReq bool) pyFieldSpec {
	rawAccess := fmt.Sprintf("data.get(%q)", propName)

	// Reference to another component schema.
	if s.Ref != "" {
		refName := strings.TrimPrefix(s.Ref, "#/components/schemas/")
		if isReq {
			return pyFieldSpec{
				Name:       propName,
				PyType:     refName,
				Default:    fmt.Sprintf("field(default_factory=%s)", refName),
				FromDict:   fmt.Sprintf("%s.from_dict(_as_json_obj(%s))", refName, rawAccess),
				ToDictExpr: fmt.Sprintf("self.%s.to_dict()", propName),
				Optional:   false,
			}
		}
		return pyFieldSpec{
			Name:       propName,
			PyType:     refName + " | None",
			Default:    "None",
			FromDict:   fmt.Sprintf("%s.from_dict(_as_json_obj(%s)) if isinstance(%s, Mapping) else None", refName, rawAccess, rawAccess),
			ToDictExpr: fmt.Sprintf("self.%s.to_dict()", propName),
			Optional:   true,
		}
	}

	switch s.Type {
	case "string":
		if isReq {
			return pyFieldSpec{
				Name:       propName,
				PyType:     "str",
				Default:    `""`,
				FromDict:   fmt.Sprintf("_as_str(%s)", rawAccess),
				ToDictExpr: fmt.Sprintf("self.%s", propName),
				Optional:   false,
			}
		}
		return pyFieldSpec{
			Name:       propName,
			PyType:     "str | None",
			Default:    "None",
			FromDict:   fmt.Sprintf("_as_opt_str(%s)", rawAccess),
			ToDictExpr: fmt.Sprintf("self.%s", propName),
			Optional:   true,
		}

	case "integer":
		if isReq {
			return pyFieldSpec{
				Name:       propName,
				PyType:     "int",
				Default:    "0",
				FromDict:   fmt.Sprintf("_as_int(%s)", rawAccess),
				ToDictExpr: fmt.Sprintf("self.%s", propName),
				Optional:   false,
			}
		}
		return pyFieldSpec{
			Name:       propName,
			PyType:     "int | None",
			Default:    "None",
			FromDict:   fmt.Sprintf("_as_opt_int(%s)", rawAccess),
			ToDictExpr: fmt.Sprintf("self.%s", propName),
			Optional:   true,
		}

	case "number":
		if isReq {
			return pyFieldSpec{
				Name:       propName,
				PyType:     "Decimal",
				Default:    "Decimal(0)",
				FromDict:   fmt.Sprintf("_as_decimal(%s)", rawAccess),
				ToDictExpr: fmt.Sprintf("self.%s", propName),
				Optional:   false,
			}
		}
		return pyFieldSpec{
			Name:       propName,
			PyType:     "Decimal | None",
			Default:    "None",
			FromDict:   fmt.Sprintf("_as_opt_decimal(%s)", rawAccess),
			ToDictExpr: fmt.Sprintf("self.%s", propName),
			Optional:   true,
		}

	case "boolean":
		if isReq {
			return pyFieldSpec{
				Name:       propName,
				PyType:     "bool",
				Default:    "False",
				FromDict:   fmt.Sprintf("_as_bool(%s)", rawAccess),
				ToDictExpr: fmt.Sprintf("self.%s", propName),
				Optional:   false,
			}
		}
		return pyFieldSpec{
			Name:       propName,
			PyType:     "bool | None",
			Default:    "None",
			FromDict:   fmt.Sprintf("_as_opt_bool(%s)", rawAccess),
			ToDictExpr: fmt.Sprintf("self.%s", propName),
			Optional:   true,
		}

	case "array":
		return buildArrayField(schemaName, propName, s, isReq, rawAccess)

	case "object":
		if isReq {
			return pyFieldSpec{
				Name:       propName,
				PyType:     "JSONObject",
				Default:    "field(default_factory=dict)",
				FromDict:   fmt.Sprintf("_as_json_obj(%s)", rawAccess),
				ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
				Optional:   false,
			}
		}
		return pyFieldSpec{
			Name:       propName,
			PyType:     "JSONObject | None",
			Default:    "None",
			FromDict:   fmt.Sprintf("_as_opt_json_obj(%s)", rawAccess),
			ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
			Optional:   true,
		}

	default:
		return pyFieldSpec{
			Name:       propName,
			PyType:     "JSONValue",
			Default:    "None",
			FromDict:   fmt.Sprintf("_as_json_val(%s)", rawAccess),
			ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
			Optional:   !isReq,
		}
	}
}

func buildArrayField(schemaName, propName string, s Schema, isReq bool, rawAccess string) pyFieldSpec {
	if s.Items != nil {
		if s.Items.Ref != "" {
			refName := strings.TrimPrefix(s.Items.Ref, "#/components/schemas/")
			fromExpr := fmt.Sprintf("[%s.from_dict(item) for item in _as_list(%s) if isinstance(item, Mapping)]", refName, rawAccess)
			toExpr := fmt.Sprintf("[item.to_dict() for item in self.%s]", propName)
			if isReq {
				return pyFieldSpec{
					Name:       propName,
					PyType:     fmt.Sprintf("list[%s]", refName),
					Default:    "field(default_factory=list)",
					FromDict:   fromExpr,
					ToDictExpr: toExpr,
					Optional:   false,
				}
			}
			return pyFieldSpec{
				Name:       propName,
				PyType:     fmt.Sprintf("list[%s] | None", refName),
				Default:    "None",
				FromDict:   fmt.Sprintf("[%s.from_dict(item) for item in _as_list(%s) if isinstance(item, Mapping)] if isinstance(%s, list) else None", refName, rawAccess, rawAccess),
				ToDictExpr: toExpr,
				Optional:   true,
			}
		}
		if schemaName == "EscalationInput" && propName == "steps" {
			return pyFieldSpec{
				Name:       propName,
				PyType:     "list[EscalationStep]",
				Default:    "field(default_factory=list)",
				FromDict:   fmt.Sprintf("[EscalationStep.from_dict(item) for item in _as_list(%s) if isinstance(item, Mapping)]", rawAccess),
				ToDictExpr: fmt.Sprintf("[item.to_dict() for item in self.%s]", propName),
				Optional:   false,
			}
		}
		switch s.Items.Type {
		case "string":
			if isReq {
				return pyFieldSpec{
					Name:       propName,
					PyType:     "list[str]",
					Default:    "field(default_factory=list)",
					FromDict:   fmt.Sprintf("_as_str_list(%s)", rawAccess),
					ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
					Optional:   false,
				}
			}
			return pyFieldSpec{
				Name:       propName,
				PyType:     "list[str] | None",
				Default:    "None",
				FromDict:   fmt.Sprintf("_as_str_list(%s) if isinstance(%s, list) else None", rawAccess, rawAccess),
				ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
				Optional:   true,
			}
		case "integer":
			if isReq {
				return pyFieldSpec{
					Name:       propName,
					PyType:     "list[int]",
					Default:    "field(default_factory=list)",
					FromDict:   fmt.Sprintf("_as_int_list(%s)", rawAccess),
					ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
					Optional:   false,
				}
			}
			return pyFieldSpec{
				Name:       propName,
				PyType:     "list[int] | None",
				Default:    "None",
				FromDict:   fmt.Sprintf("_as_int_list(%s) if isinstance(%s, list) else None", rawAccess, rawAccess),
				ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
				Optional:   true,
			}
		}
	}

	if isReq {
		return pyFieldSpec{
			Name:       propName,
			PyType:     "list[JSONObject]",
			Default:    "field(default_factory=list)",
			FromDict:   fmt.Sprintf("_as_json_obj_list(%s)", rawAccess),
			ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
			Optional:   false,
		}
	}
	return pyFieldSpec{
		Name:       propName,
		PyType:     "list[JSONObject] | None",
		Default:    "None",
		FromDict:   fmt.Sprintf("_as_json_obj_list(%s) if isinstance(%s, list) else None", rawAccess, rawAccess),
		ToDictExpr: fmt.Sprintf("_as_json_val(self.%s)", propName),
		Optional:   true,
	}
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
	case "AuditEntry":
		return map[string]bool{
			"id": true, "actor": true, "action": true, "target_type": true, "target_id": true, "created_at": true,
		}
	case "AuditPage":
		return map[string]bool{"entries": true}
	case "DailyAlertCount":
		return map[string]bool{"day": true, "count": true}
	case "DailyAlertCountsResponse":
		return map[string]bool{"timezone": true, "days": true}
	default:
		return nil
	}
}

func writeRouteTable(b *bytes.Buffer, doc *OpenAPIDoc) {
	b.WriteString("OPENAPI_OPERATIONS: Final[tuple[tuple[str, str, str, str], ...]] = (\n")
	for _, rb := range knownRouteBindings {
		summary := ""
		if item, ok := doc.Paths[rb.Path]; ok {
			if op := operationsMap(item)[rb.Method]; op != nil {
				summary = op.Summary
			}
		}
		fmt.Fprintf(b, "    (%q, %q, %q, %q),\n", rb.Method, rb.Path, rb.ClientMethod, summary)
	}
	b.WriteString(")\n")
}
