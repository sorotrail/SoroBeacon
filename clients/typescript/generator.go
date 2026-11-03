// Package typescript generates and verifies the TypeScript client for the
// SoroBeacon HTTP API from internal/api/openapi.json.
//
//go:generate go test . -run TestTypeScriptClientMatchesOpenAPISpec -update
package typescript

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

// DefaultOutputPath returns the path to the generated TypeScript client entry point.
func DefaultOutputPath() string {
	return filepath.Join(PackageDir(), "src", "index.ts")
}

// GenerateFromFile reads internal/api/openapi.json and returns the deterministic
// TypeScript source for clients/typescript/src/index.ts.
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
	{"GET", "/monitors", "listMonitors"},
	{"POST", "/monitors", "createMonitor"},
	{"POST", "/monitors/bulk", "bulkUpdateMonitors"},
	{"POST", "/monitors/import", "importMonitors"},
	{"GET", "/monitors/trash", "listTrashMonitors"},
	{"GET", "/monitors/{id}", "getMonitor"},
	{"PATCH", "/monitors/{id}", "updateMonitor"},
	{"DELETE", "/monitors/{id}", "deleteMonitor"},
	{"POST", "/monitors/{id}/soft-delete", "softDeleteMonitor"},
	{"POST", "/monitors/{id}/restore", "restoreMonitor"},
	{"POST", "/monitors/{id}/duplicate", "duplicateMonitor"},
	{"GET", "/monitors/{id}/stats", "getMonitorStats"},
	{"GET", "/monitors/{id}/escalation", "getMonitorEscalation"},
	{"PUT", "/monitors/{id}/escalation", "setMonitorEscalation"},
	{"DELETE", "/monitors/{id}/escalation", "deleteMonitorEscalation"},
	{"GET", "/monitors/{id}/rules", "listRules"},
	{"POST", "/monitors/{id}/rules", "createRule"},
	{"POST", "/monitors/{id}/rules/bulk", "bulkCreateRules"},
	{"POST", "/monitors/{id}/rules/dry-run", "dryRunRule"},
	{"PATCH", "/monitors/{id}/rules/{ruleID}", "updateRule"},
	{"DELETE", "/monitors/{id}/rules/{ruleID}", "deleteRule"},
	{"GET", "/channels", "listChannels"},
	{"POST", "/channels", "createChannel"},
	{"POST", "/channels/bulk", "bulkUpdateChannels"},
	{"GET", "/channels/{id}", "getChannel"},
	{"PATCH", "/channels/{id}", "updateChannel"},
	{"DELETE", "/channels/{id}", "deleteChannel"},
	{"GET", "/channels/{id}/stats", "getChannelStats"},
	{"POST", "/channels/{id}/duplicate", "duplicateChannel"},
	{"POST", "/channels/{id}/test", "testChannel"},
	{"GET", "/channels/{id}/keys", "listChannelKeys"},
	{"POST", "/channels/{id}/keys/rotate", "rotateChannelKey"},
	{"POST", "/channels/{id}/keys/{keyID}/retire", "retireChannelKey"},
	{"POST", "/channels/{id}/keys/{keyID}/revoke", "revokeChannelKey"},
	{"GET", "/maintenance-windows", "listMaintenanceWindows"},
	{"POST", "/maintenance-windows", "createMaintenanceWindow"},
	{"GET", "/maintenance-windows/{id}", "getMaintenanceWindow"},
	{"PATCH", "/maintenance-windows/{id}", "updateMaintenanceWindow"},
	{"DELETE", "/maintenance-windows/{id}", "deleteMaintenanceWindow"},
	{"GET", "/inhibitions", "listInhibitions"},
	{"POST", "/inhibitions", "createInhibition"},
	{"DELETE", "/inhibitions/{sourceID}/{targetID}", "deleteInhibition"},
	{"GET", "/alerts", "listAlerts"},
	{"GET", "/alerts/stream", "streamAlerts"},
	{"GET", "/alerts.csv", "exportAlertsCSV"},
	{"GET", "/alerts/export", "exportAlertsNDJSON"},
	{"POST", "/alerts/{id}/acknowledge", "acknowledgeAlert"},
	{"GET", "/alerts/{id}/deliveries", "listAlertDeliveries"},
	{"POST", "/alerts/{id}/deliveries/{channelID}/retry", "retryAlertDelivery"},
	{"GET", "/dead-letters", "listDeadLetters"},
	{"GET", "/dead-letters/groups", "listDeadLetterGroups"},
	{"POST", "/dead-letters/bulk/redrive", "bulkRedriveDeadLetters"},
	{"POST", "/dead-letters/bulk/discard", "bulkDiscardDeadLetters"},
	{"POST", "/dead-letters/{id}/redrive", "redriveDeadLetter"},
	{"POST", "/dead-letters/{id}/discard", "discardDeadLetter"},
	{"POST", "/ingest", "ingestFederatedAlert"},
	{"POST", "/triggers", "triggerEvaluation"},
	{"GET", "/health", "getHealth"},
	{"GET", "/livez", "getLivez"},
	{"GET", "/readyz", "getReadyz"},
	{"GET", "/poller", "getPollerStatus"},
	{"GET", "/version", "getVersion"},
	{"GET", "/rule-types", "listRuleTypes"},
	{"GET", "/stats", "getStats"},
	{"GET", "/stats/alerts-daily", "getDailyAlertCounts"},
	{"GET", "/audit", "listAuditLogs"},
	{"GET", "/templates", "listTemplates"},
	{"POST", "/templates", "createTemplate"},
	{"GET", "/templates/{id}", "getTemplate"},
	{"PATCH", "/templates/{id}", "updateTemplate"},
	{"DELETE", "/templates/{id}", "deleteTemplate"},
	{"POST", "/templates/{id}/instantiate", "instantiateTemplate"},
	{"POST", "/templates/{id}/instantiate/bulk", "bulkInstantiateTemplate"},
	{"GET", "/tokens", "listTokens"},
	{"POST", "/tokens", "createToken"},
	{"GET", "/tokens/{id}", "getToken"},
	{"POST", "/tokens/{id}/revoke", "revokeToken"},
	{"POST", "/tokens/{id}/rotate", "rotateToken"},
	{"POST", "/tokens/{id}/abandon", "abandonTokenRotation"},
	{"GET", "/graphql/operations", "listGraphQLOperations"},
	{"POST", "/graphql/operations", "registerGraphQLOperation"},
	{"GET", "/graphql/operations/{id}", "getGraphQLOperation"},
	{"DELETE", "/graphql/operations/{id}", "deleteGraphQLOperation"},
}

// Generate parses the OpenAPI JSON bytes, validates all component references and
// route mappings, and emits the complete TypeScript SDK source.
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
	writeScalarAndJsonTypes(&b)
	if err := writeComponentSchemas(&b, &doc); err != nil {
		return nil, err
	}
	writeSupplementaryEndpointTypes(&b)
	writeWideIntAndErrorRuntime(&b, &doc)
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
			return fmt.Errorf("openapi.json operation %q has no TypeScript client method binding", key)
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
	fmt.Fprintf(b, `// Code generated from internal/api/openapi.json (%s v%s). DO NOT EDIT.
// Regenerate with: go generate ./clients/typescript/...

`, doc.Info.Title, doc.Info.Version)
}

func writeScalarAndJsonTypes(b *bytes.Buffer) {
	b.WriteString(`/**
 * 64-bit integer input value (resource IDs, ledger sequence numbers, counters,
 * and wide Soroban integers). Callers may pass a safe JavaScript number, a
 * bigint, or a base-10 integer string so values above Number.MAX_SAFE_INTEGER
 * (2^53 - 1) never lose precision.
 */
export type Int64 = number | bigint | ` + "`${number}`" + `;

/**
 * 64-bit integer value decoded from JSON responses. Values within IEEE-754 safe
 * integer bounds are decoded as ` + "`number`" + `; values outside
 * [Number.MIN_SAFE_INTEGER, Number.MAX_SAFE_INTEGER] are preserved losslessly
 * as decimal strings (` + "`\"9007199254740993\"`" + `) by ` + "`parseJSONPreservingWideInts`" + `.
 */
export type Int64Value = number | ` + "`${number}`" + `;

/** RFC 3339 / ISO-8601 timestamp string (e.g. "2026-01-15T12:34:56Z"). */
export type ISODateTimeString = string;

/** Calendar date string in YYYY-MM-DD format (UTC). */
export type ISODateString = string;

/** Structured JSON primitive value. */
export type JsonPrimitive = string | number | boolean | null;

/** Structured JSON value (never an opaque unparsed string). */
export type JsonValue = JsonPrimitive | JsonObject | JsonValue[];

/** Structured JSON object map. */
export interface JsonObject {
  [key: string]: JsonValue | undefined;
}

/**
 * Decoded Soroban contract event payload attached to an Alert.
 * Large integers inside decoded topics or event values (> 2^53 - 1, such as
 * i128/u128 token amounts) are preserved as exact decimal strings.
 */
export interface AlertPayload extends JsonObject {
  contract_id?: string;
  event_name?: string;
  ledger?: Int64Value;
  topics?: JsonValue[];
  data?: JsonValue;
  value?: JsonValue;
}

`)
}

func writeComponentSchemas(b *bytes.Buffer, doc *OpenAPIDoc) error {
	names := make([]string, 0, len(doc.Components.Schemas))
	for name := range doc.Components.Schemas {
		names = append(names, name)
	}
	sort.Strings(names)

	// Emit named union types for schemas with enum properties first.
	if errSchema, ok := doc.Components.Schemas["Error"]; ok {
		if codeProp, ok := errSchema.Properties["code"]; ok && len(codeProp.Enum) > 0 {
			b.WriteString("/** Stable machine-readable error codes returned in the API Error envelope. */\n")
			fmt.Fprintf(b, "export type ErrorCode =\n  | %s;\n\n", joinQuoted(codeProp.Enum, "\n  | "))
		}
	}
	if ruleSchema, ok := doc.Components.Schemas["Rule"]; ok {
		if typeProp, ok := ruleSchema.Properties["type"]; ok && len(typeProp.Enum) > 0 {
			b.WriteString("/** Built-in rule evaluation types. */\n")
			fmt.Fprintf(b, "export type RuleTypeName =\n  | %s;\n\n", joinQuoted(typeProp.Enum, "\n  | "))
		}
	}
	if chSchema, ok := doc.Components.Schemas["ChannelInput"]; ok {
		if typeProp, ok := chSchema.Properties["type"]; ok && len(typeProp.Enum) > 0 {
			b.WriteString("/** Supported notification delivery channel types. */\n")
			fmt.Fprintf(b, "export type ChannelType =\n  | %s;\n\n", joinQuoted(typeProp.Enum, "\n  | "))
		}
	}

	for _, name := range names {
		schema := doc.Components.Schemas[name]
		if schema.Description != "" {
			writeDocComment(b, schema.Description, "")
		}
		fmt.Fprintf(b, "export interface %s {\n", name)

		reqSet := map[string]bool{}
		for _, r := range schema.Required {
			reqSet[r] = true
		}

		// For read models whose write-only fields are always redacted by the
		// server (Channel.config and APIToken.token), keep the property
		// requiredness aligned with actual server JSON serialization so callers
		// get ergonomic field access while optional/nullable fields stay typed.
		alwaysPresentReadFields := readModelGuaranteedFields(name)

		propNames := make([]string, 0, len(schema.Properties))
		for propName := range schema.Properties {
			propNames = append(propNames, propName)
		}
		sort.Strings(propNames)

		for _, propName := range propNames {
			prop := schema.Properties[propName]
			if prop.Description != "" {
				writeDocComment(b, prop.Description, "  ")
			}
			isReq := reqSet[propName] || alwaysPresentReadFields[propName]
			optMark := "?"
			if isReq {
				optMark = ""
			}
			tsType := schemaPropertyType(name, propName, prop, isInputSchema(name))
			nullableSuffix := ""
			if strings.Contains(strings.ToLower(prop.Description), "null when") ||
				(name == "APIToken" || name == "CreatedAPIToken") && (propName == "expires_at" || propName == "last_used_at" || propName == "revoked_at" || propName == "rotated_at") {
				nullableSuffix = " | null"
			}
			fmt.Fprintf(b, "  %s%s: %s%s;\n", propName, optMark, tsType, nullableSuffix)
		}

		// Explicitly forbid redacted secrets on read models so TypeScript catches
		// any attempt to read `channel.config` or `apiToken.token` at compile time.
		if name == "Channel" {
			b.WriteString("  /**\n   * Redacted on read: channel configuration contains secrets (webhook URLs,\n   * bot tokens, SMTP credentials) and is accepted only on ChannelInput.\n   */\n")
			b.WriteString("  config?: never;\n")
		}
		if name == "APIToken" {
			b.WriteString("  /**\n   * Redacted on read: the plaintext token secret is returned only once in\n   * CreatedAPIToken when minting or rotating a token.\n   */\n")
			b.WriteString("  token?: never;\n")
		}

		b.WriteString("}\n\n")
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
	return schemaToTSType(s, isInput)
}

func schemaToTSType(s Schema, isInput bool) string {
	if s.Ref != "" {
		return strings.TrimPrefix(s.Ref, "#/components/schemas/")
	}
	if len(s.Enum) > 0 {
		return joinQuoted(s.Enum, " | ")
	}
	switch s.Type {
	case "string":
		switch s.Format {
		case "date-time":
			return "ISODateTimeString"
		case "date":
			return "ISODateString"
		default:
			return "string"
		}
	case "integer":
		if s.Format == "int64" {
			if isInput {
				return "Int64"
			}
			return "Int64Value"
		}
		return "number"
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		if s.Items == nil {
			return "JsonValue[]"
		}
		itemType := schemaToTSType(*s.Items, isInput)
		if strings.Contains(itemType, " | ") {
			return "(" + itemType + ")[]"
		}
		return itemType + "[]"
	case "object":
		if len(s.Properties) > 0 {
			reqSet := map[string]bool{}
			for _, r := range s.Required {
				reqSet[r] = true
			}
			keys := make([]string, 0, len(s.Properties))
			for k := range s.Properties {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				opt := "?"
				if reqSet[k] {
					opt = ""
				}
				parts = append(parts, fmt.Sprintf("%s%s: %s", k, opt, schemaToTSType(s.Properties[k], isInput)))
			}
			return "{ " + strings.Join(parts, "; ") + " }"
		}
		if s.AdditionalProperties != nil {
			return fmt.Sprintf("Record<string, %s>", schemaToTSType(*s.AdditionalProperties, isInput))
		}
		return "JsonObject"
	default:
		return "JsonValue"
	}
}

func writeSupplementaryEndpointTypes(b *bytes.Buffer) {
	b.WriteString(`/** Partial update payload for PATCH /monitors/{id}. */
export type MonitorUpdateInput = Partial<MonitorInput>;

/** Partial update payload for PATCH /rules/{ruleID}. */
export type RuleUpdateInput = Partial<RuleInput>;

/**
 * Partial update payload for PATCH /channels/{id}.
 * ` + "`config`" + ` is accepted on write when rotating or updating channel credentials,
 * and is never echoed back in Channel responses.
 */
export type ChannelUpdateInput = Partial<ChannelInput>;

/** Partial update payload for PATCH /maintenance-windows/{id}. */
export type MaintenanceWindowUpdateInput = Partial<MaintenanceWindowInput>;

/** Partial update payload for PATCH /templates/{id}. */
export type MonitorTemplateUpdateInput = Partial<MonitorTemplateInput>;

/** Query parameters for GET /monitors and GET /monitors/trash. */
export interface ListMonitorsParams {
  q?: string;
  enabled?: boolean;
  tag?: string;
  network?: string;
  sort?: "id_asc" | "id_desc" | "name_asc" | "name_desc";
  limit?: number;
  cursor?: string;
}

/** Query parameters for GET /channels. */
export interface ListChannelsParams {
  q?: string;
  type?: ChannelType;
  enabled?: boolean;
  sort?: "id_asc" | "id_desc" | "name_asc" | "name_desc";
  limit?: number;
  cursor?: string;
}

/** Query parameters for GET /alerts, GET /alerts.csv, and GET /alerts/export. */
export interface ListAlertsParams {
  monitor_id?: Int64;
  rule_id?: Int64;
  contract_id?: string;
  q?: string;
  sort?: "created_at_desc" | "created_at_asc";
  from?: ISODateTimeString;
  to?: ISODateTimeString;
  network?: string;
  limit?: number;
  cursor?: string;
}

/** Query parameters for GET /maintenance-windows. */
export interface ListMaintenanceWindowsParams {
  active?: boolean;
  upcoming?: boolean;
  limit?: number;
}

/** Query parameters for GET /audit. */
export interface ListAuditParams {
  target_type?: "monitor" | "rule" | "channel" | "alert";
  target_id?: Int64;
  actor?: string;
  cursor?: Int64;
  from?: ISODateTimeString;
  to?: ISODateTimeString;
  limit?: number;
}

/** Single entry in the append-only configuration audit log (GET /audit). */
export interface AuditEntry {
  id: Int64Value;
  actor: string;
  action: "create" | "update" | "delete" | string;
  target_type: "monitor" | "rule" | "channel" | "alert" | string;
  target_id: Int64Value;
  diff?: JsonObject;
  created_at: ISODateTimeString;
}

/** Response envelope for GET /audit. */
export interface AuditPage {
  entries: AuditEntry[];
}

/** Response envelope for GET /stats/alerts-daily. */
export interface DailyAlertCountsResponse {
  timezone: string;
  days: { day: ISODateString; count: Int64Value }[];
}

/** Service statistics returned by GET /stats. */
export interface ServiceStats extends JsonObject {
  monitors?: Int64Value;
  rules?: Int64Value;
  channels?: Int64Value;
  alerts?: Int64Value;
  deliveries_succeeded?: Int64Value;
  deliveries_failed?: Int64Value;
  last_processed_ledger?: Int64Value;
  retention_gap?: {
    network: string;
    from_ledger: Int64Value;
    to_ledger: Int64Value;
    detected_at: ISODateTimeString;
  };
}

/** Request payload for POST /ingest (federated alert ingestion). */
export interface FederatedIngestInput {
  alert: Alert;
  origin: string;
  hop_count?: number;
}

/** Response payload for POST /ingest. */
export interface FederatedIngestResult {
  outcome: "created" | "duplicate" | "suppressed";
  alert_id?: Int64Value;
  origin?: string;
}

/** Request payload for POST /triggers. */
export interface TriggerEvaluationInput {
  monitor_id: Int64;
  event?: string;
  source?: string;
}

/** Response payload for POST /triggers. */
export interface TriggerEvaluationResult {
  alert_id?: Int64Value;
  note?: string;
}

/** Optional rotation parameters for POST /tokens/{id}/rotate. */
export interface RotateTokenInput {
  name?: string;
  scopes?: string[];
  overlap?: string;
  expires_in?: string;
}

/** Channel webhook signing key metadata (secret omitted on read). */
export interface ChannelSigningKey extends JsonObject {
  id?: string;
  key_id?: string;
  status?: string;
  created_at?: ISODateTimeString;
  retired_at?: ISODateTimeString | null;
  revoked_at?: ISODateTimeString | null;
  secret?: never;
}

/** Created/rotated channel signing key response (secret returned once). */
export interface RotatedChannelSigningKey extends JsonObject {
  id?: string;
  key_id?: string;
  secret?: string;
  created_at?: ISODateTimeString;
}

/** Persisted GraphQL operation entry. */
export interface PersistedGraphQLOperation extends JsonObject {
  id?: string;
  name?: string;
  query?: string;
  created_at?: ISODateTimeString;
}

`)
}

func writeWideIntAndErrorRuntime(b *bytes.Buffer, doc *OpenAPIDoc) {
	b.WriteString(`/**
 * Converts an Int64 value (number, bigint, or decimal string) into a native
 * ` + "`bigint`" + ` without IEEE-754 precision loss.
 */
export function toBigInt(value: Int64): bigint {
  if (typeof value === "bigint") {
    return value;
  }
  if (typeof value === "number") {
    if (!Number.isInteger(value) || !Number.isSafeInteger(value)) {
      throw new RangeError(
        ` + "`unsafe integer number ${value}; pass a bigint or decimal string instead`" + `,
      );
    }
    return BigInt(value);
  }
  return BigInt(value);
}

const MAX_SAFE_BIGINT = BigInt(Number.MAX_SAFE_INTEGER);
const MIN_SAFE_BIGINT = BigInt(Number.MIN_SAFE_INTEGER);

/**
 * Parses a JSON payload while preserving any integer literal outside IEEE-754
 * safe integer bounds ([Number.MIN_SAFE_INTEGER, Number.MAX_SAFE_INTEGER]) as
 * an exact decimal string before JSON.parse can round its low bits.
 */
export function parseJSONPreservingWideInts<T = unknown>(raw: string): T {
  const safeJson = quoteUnsafeIntegersInJSON(raw);
  return JSON.parse(safeJson) as T;
}

function quoteUnsafeIntegersInJSON(raw: string): string {
  let out = "";
  let i = 0;
  const n = raw.length;

  while (i < n) {
    const ch = raw[i];
    if (ch === '"') {
      // Copy string literal verbatim, respecting escapes.
      const start = i;
      i++;
      while (i < n) {
        if (raw[i] === "\\") {
          i += 2;
          continue;
        }
        if (raw[i] === '"') {
          i++;
          break;
        }
        i++;
      }
      out += raw.slice(start, i);
      continue;
    }

    if (ch === "-" || (ch >= "0" && ch <= "9")) {
      const start = i;
      if (raw[i] === "-") {
        i++;
      }
      let digitsStart = i;
      while (i < n && raw[i] >= "0" && raw[i] <= "9") {
        i++;
      }
      const hasDigits = i > digitsStart;
      let isFloat = false;
      if (i < n && raw[i] === ".") {
        isFloat = true;
        i++;
        while (i < n && raw[i] >= "0" && raw[i] <= "9") {
          i++;
        }
      }
      if (i < n && (raw[i] === "e" || raw[i] === "E")) {
        isFloat = true;
        i++;
        if (i < n && (raw[i] === "+" || raw[i] === "-")) {
          i++;
        }
        while (i < n && raw[i] >= "0" && raw[i] <= "9") {
          i++;
        }
      }
      const token = raw.slice(start, i);
      if (hasDigits && !isFloat && token.length >= 16) {
        try {
          const bi = BigInt(token);
          if (bi > MAX_SAFE_BIGINT || bi < MIN_SAFE_BIGINT) {
            out += '"' + token + '"';
            continue;
          }
        } catch {
          // Fall through and emit original token.
        }
      }
      out += token;
      continue;
    }

    out += ch;
    i++;
  }
  return out;
}

const BIGINT_SENTINEL_PREFIX = "__SOROBEACON_BIGINT__:";
const BIGINT_SENTINEL_REGEX = /"__SOROBEACON_BIGINT__:(-?\d+)"/g;

/**
 * Serializes a request body to JSON, emitting ` + "`bigint`" + ` values as exact
 * JSON integer tokens so 64-bit IDs and ledger numbers are transmitted as
 * numbers on the wire without precision loss.
 */
export function stringifyJSONWithWideInts(value: unknown): string {
  const encoded = JSON.stringify(value, (_key, val) =>
    typeof val === "bigint" ? BIGINT_SENTINEL_PREFIX + val.toString() : val,
  );
  return encoded.replace(BIGINT_SENTINEL_REGEX, "$1");
}

/**
 * Typed error raised when the SoroBeacon API returns a non-2xx HTTP response.
 * Exposes the HTTP status, machine-readable ` + "`code`" + ` from the Error taxonomy,
 * correlation ` + "`requestId`" + `, and field-level validation ` + "`details`" + `.
 */
export class SoroBeaconAPIError extends Error {
  readonly status: number;
  readonly code?: ErrorCode | string;
  readonly requestId?: string;
  readonly details?: ErrorDetail[];
  readonly body?: Error;

  constructor(
    status: number,
    envelope: {
      error: string;
      code?: ErrorCode | string;
      request_id?: string;
      details?: ErrorDetail[];
    },
  ) {
    super(envelope.error || ` + "`HTTP ${status}`" + `);
    this.name = "SoroBeaconAPIError";
    this.status = status;
    this.code = envelope.code;
    this.requestId = envelope.request_id;
    this.details = envelope.details;
    Object.setPrototypeOf(this, SoroBeaconAPIError.prototype);
  }

  static is(err: unknown, code?: ErrorCode): err is SoroBeaconAPIError {
    if (!(err instanceof SoroBeaconAPIError)) {
      return false;
    }
    return code === undefined || err.code === code;
  }

  isNotFound(): boolean {
    return this.code === "NOT_FOUND" || this.status === 404;
  }

  isAlreadyExists(): boolean {
    return this.code === "ALREADY_EXISTS";
  }

  isInvalidArgument(): boolean {
    return this.code === "INVALID_ARGUMENT" || this.status === 400 || this.status === 422;
  }

  isConflict(): boolean {
    return this.code === "CONFLICT" || this.status === 409;
  }

  isUnauthenticated(): boolean {
    return this.code === "UNAUTHENTICATED" || this.status === 401;
  }

  isPermissionDenied(): boolean {
    return this.code === "PERMISSION_DENIED" || this.status === 403;
  }

  isRateLimited(): boolean {
    return this.code === "RATE_LIMITED" || this.status === 429;
  }

  isQuotaExceeded(): boolean {
    return this.code === "QUOTA_EXCEEDED";
  }

  isPayloadTooLarge(): boolean {
    return this.code === "PAYLOAD_TOO_LARGE" || this.status === 413;
  }

  isServiceUnavailable(): boolean {
    return this.code === "SERVICE_UNAVAILABLE" || this.status === 503;
  }
}

`)
}

func writeRouteTable(b *bytes.Buffer, doc *OpenAPIDoc) {
	b.WriteString("/** Complete table of OpenAPI operations covered by SoroBeaconClient. */\n")
	b.WriteString("export const OPENAPI_OPERATIONS = [\n")
	for _, rb := range knownRouteBindings {
		summary := ""
		if item, ok := doc.Paths[rb.Path]; ok {
			if op := operationsMap(item)[rb.Method]; op != nil {
				summary = op.Summary
			}
		}
		fmt.Fprintf(b, "  { method: %q, path: %q, clientMethod: %q, summary: %q },\n", rb.Method, rb.Path, rb.ClientMethod, summary)
	}
	b.WriteString("] as const;\n\n")
}

func writeClientClass(b *bytes.Buffer) {
	b.WriteString(`/** Configuration options for SoroBeaconClient. */
export interface SoroBeaconClientOptions {
  /**
   * Base URL of the SoroBeacon server (e.g. "http://localhost:8080" or
   * "http://localhost:8080/api/v1"). If the path does not already end in
   * "/api/v1", the client appends "/api/v1" automatically.
   */
  baseUrl: string;
  /** Bearer token (static API_TOKEN, workspace token, or minted sb_... token). */
  token?: string;
  /** Optional custom fetch implementation (defaults to globalThis.fetch). */
  fetch?: typeof fetch;
  /** Additional HTTP headers included on every request. */
  headers?: Record<string, string>;
}

/** Per-request options (such as AbortSignal or If-Match / If-None-Match headers). */
export interface RequestOptions {
  signal?: AbortSignal;
  headers?: Record<string, string>;
}

/**
 * Generated TypeScript client for the SoroBeacon HTTP API (/api/v1).
 */
export class SoroBeaconClient {
  private readonly baseUrl: string;
  private readonly token?: string;
  private readonly fetchImpl: typeof fetch;
  private readonly defaultHeaders: Record<string, string>;

  constructor(options: SoroBeaconClientOptions) {
    const trimmed = options.baseUrl.replace(/\/+$/, "");
    this.baseUrl = trimmed.endsWith("/api/v1") ? trimmed : ` + "`${trimmed}/api/v1`" + `;
    this.token = options.token;
    this.fetchImpl = options.fetch ?? globalThis.fetch.bind(globalThis);
    this.defaultHeaders = { ...(options.headers ?? {}) };
  }

  private buildUrl(path: string, query?: Record<string, unknown>): string {
    const url = new URL(this.baseUrl + path);
    if (query) {
      for (const [k, v] of Object.entries(query)) {
        if (v !== undefined && v !== null) {
          url.searchParams.set(k, String(v));
        }
      }
    }
    return url.toString();
  }

  private buildHeaders(
    extra?: Record<string, string>,
    hasJsonBody?: boolean,
  ): Record<string, string> {
    const headers: Record<string, string> = {
      Accept: "application/json",
      ...this.defaultHeaders,
      ...(extra ?? {}),
    };
    if (hasJsonBody && !headers["Content-Type"]) {
      headers["Content-Type"] = "application/json";
    }
    if (this.token && !headers["Authorization"]) {
      headers["Authorization"] = ` + "`Bearer ${this.token}`" + `;
    }
    return headers;
  }

  private async request<T>(
    method: string,
    path: string,
    opts?: {
      query?: Record<string, unknown>;
      body?: unknown;
      rawBody?: string;
      contentType?: string;
      accept?: string;
      parseAsText?: boolean;
      requestOptions?: RequestOptions;
    },
  ): Promise<T> {
    const hasJsonBody = opts?.body !== undefined;
    const headers = this.buildHeaders(opts?.requestOptions?.headers, hasJsonBody);
    if (opts?.contentType) {
      headers["Content-Type"] = opts.contentType;
    }
    if (opts?.accept) {
      headers["Accept"] = opts.accept;
    }

    const res = await this.fetchImpl(this.buildUrl(path, opts?.query), {
      method,
      headers,
      body: hasJsonBody
        ? stringifyJSONWithWideInts(opts!.body)
        : opts?.rawBody,
      signal: opts?.requestOptions?.signal,
    });

    const rawText = await res.text();
    if (!res.ok) {
      let envelope: {
        error: string;
        code?: ErrorCode | string;
        request_id?: string;
        details?: ErrorDetail[];
      } = { error: rawText || res.statusText || ` + "`HTTP ${res.status}`" + ` };
      if (rawText) {
        try {
          const parsed = parseJSONPreservingWideInts<Record<string, unknown>>(rawText);
          if (parsed && typeof parsed.error === "string") {
            envelope = {
              error: parsed.error,
              code: typeof parsed.code === "string" ? parsed.code : undefined,
              request_id:
                typeof parsed.request_id === "string"
                  ? parsed.request_id
                  : (res.headers.get("X-Request-ID") ?? undefined),
              details: Array.isArray(parsed.details)
                ? (parsed.details as ErrorDetail[])
                : undefined,
            };
          }
        } catch {
          // Keep plain-text fallback envelope.
        }
      }
      if (!envelope.request_id) {
        const hdrReqId = res.headers.get("X-Request-ID");
        if (hdrReqId) {
          envelope.request_id = hdrReqId;
        }
      }
      throw new SoroBeaconAPIError(res.status, envelope);
    }

    if (res.status === 204 || rawText === "") {
      return undefined as T;
    }
    if (opts?.parseAsText) {
      return rawText as unknown as T;
    }
    return parseJSONPreservingWideInts<T>(rawText);
  }

  // ---------------------------------------------------------------------------
  // Monitors
  // ---------------------------------------------------------------------------

  async listMonitors(
    params?: ListMonitorsParams,
    options?: RequestOptions,
  ): Promise<MonitorPage> {
    return this.request<MonitorPage>("GET", "/monitors", {
      query: params as Record<string, unknown>,
      requestOptions: options,
    });
  }

  /**
   * Async iterator that transparently pages through GET /monitors using
   * ` + "`next_cursor`" + ` until all matching monitors have been yielded.
   */
  async *paginateMonitors(
    params?: Omit<ListMonitorsParams, "cursor">,
    options?: RequestOptions,
  ): AsyncGenerator<Monitor, void, unknown> {
    let cursor: string | undefined = undefined;
    do {
      const page = await this.listMonitors({ ...params, cursor }, options);
      for (const item of page.monitors ?? []) {
        yield item;
      }
      cursor = page.next_cursor || undefined;
    } while (cursor);
  }

  async createMonitor(
    input: MonitorInput,
    options?: RequestOptions,
  ): Promise<Monitor> {
    return this.request<Monitor>("POST", "/monitors", {
      body: input,
      requestOptions: options,
    });
  }

  async getMonitor(id: Int64, options?: RequestOptions): Promise<Monitor> {
    return this.request<Monitor>(
      "GET",
      ` + "`/monitors/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async updateMonitor(
    id: Int64,
    input: MonitorUpdateInput,
    options?: RequestOptions,
  ): Promise<Monitor> {
    return this.request<Monitor>(
      "PATCH",
      ` + "`/monitors/${encodeURIComponent(String(id))}`" + `,
      { body: input, requestOptions: options },
    );
  }

  async deleteMonitor(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "DELETE",
      ` + "`/monitors/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async bulkUpdateMonitors(
    input: BulkMonitorInput,
    options?: RequestOptions,
  ): Promise<BulkMonitorResult> {
    return this.request<BulkMonitorResult>("POST", "/monitors/bulk", {
      body: input,
      requestOptions: options,
    });
  }

  async importMonitors(
    input: ImportInput | string,
    query?: { name?: string },
    options?: RequestOptions,
  ): Promise<{ total?: number; results?: ImportRowResult[] }> {
    if (typeof input === "string") {
      return this.request("POST", "/monitors/import", {
        query,
        rawBody: input,
        contentType: "text/csv",
        requestOptions: options,
      });
    }
    return this.request("POST", "/monitors/import", {
      query,
      body: input,
      requestOptions: options,
    });
  }

  async listTrashMonitors(
    params?: ListMonitorsParams,
    options?: RequestOptions,
  ): Promise<MonitorPage> {
    return this.request<MonitorPage>("GET", "/monitors/trash", {
      query: params as Record<string, unknown>,
      requestOptions: options,
    });
  }

  async *paginateTrashMonitors(
    params?: Omit<ListMonitorsParams, "cursor">,
    options?: RequestOptions,
  ): AsyncGenerator<Monitor, void, unknown> {
    let cursor: string | undefined = undefined;
    do {
      const page = await this.listTrashMonitors({ ...params, cursor }, options);
      for (const item of page.monitors ?? []) {
        yield item;
      }
      cursor = page.next_cursor || undefined;
    } while (cursor);
  }

  async softDeleteMonitor(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "POST",
      ` + "`/monitors/${encodeURIComponent(String(id))}/soft-delete`" + `,
      { requestOptions: options },
    );
  }

  async restoreMonitor(id: Int64, options?: RequestOptions): Promise<Monitor> {
    return this.request<Monitor>(
      "POST",
      ` + "`/monitors/${encodeURIComponent(String(id))}/restore`" + `,
      { requestOptions: options },
    );
  }

  async duplicateMonitor(
    id: Int64,
    input?: { name?: string },
    options?: RequestOptions,
  ): Promise<Monitor> {
    return this.request<Monitor>(
      "POST",
      ` + "`/monitors/${encodeURIComponent(String(id))}/duplicate`" + `,
      { body: input ?? {}, requestOptions: options },
    );
  }

  async getMonitorStats(
    id: Int64,
    options?: RequestOptions,
  ): Promise<MonitorStats> {
    return this.request<MonitorStats>(
      "GET",
      ` + "`/monitors/${encodeURIComponent(String(id))}/stats`" + `,
      { requestOptions: options },
    );
  }

  // ---------------------------------------------------------------------------
  // Escalation Policies & Rules
  // ---------------------------------------------------------------------------

  async getMonitorEscalation(
    monitorId: Int64,
    options?: RequestOptions,
  ): Promise<EscalationPolicy> {
    return this.request<EscalationPolicy>(
      "GET",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/escalation`" + `,
      { requestOptions: options },
    );
  }

  async setMonitorEscalation(
    monitorId: Int64,
    input: EscalationInput,
    options?: RequestOptions,
  ): Promise<EscalationPolicy> {
    return this.request<EscalationPolicy>(
      "PUT",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/escalation`" + `,
      { body: input, requestOptions: options },
    );
  }

  async deleteMonitorEscalation(
    monitorId: Int64,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "DELETE",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/escalation`" + `,
      { requestOptions: options },
    );
  }

  async listRules(
    monitorId: Int64,
    options?: RequestOptions,
  ): Promise<Rule[]> {
    return this.request<Rule[]>(
      "GET",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/rules`" + `,
      { requestOptions: options },
    );
  }

  async createRule(
    monitorId: Int64,
    input: RuleInput,
    options?: RequestOptions,
  ): Promise<Rule> {
    return this.request<Rule>(
      "POST",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/rules`" + `,
      { body: input, requestOptions: options },
    );
  }

  async bulkCreateRules(
    monitorId: Int64,
    rules: RuleInput[],
    options?: RequestOptions,
  ): Promise<{ rules: Rule[] }> {
    return this.request<{ rules: Rule[] }>(
      "POST",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/rules/bulk`" + `,
      { body: { rules }, requestOptions: options },
    );
  }

  async dryRunRule(
    monitorId: Int64,
    input: RuleInput,
    query?: { limit?: number },
    options?: RequestOptions,
  ): Promise<DryRunResult> {
    return this.request<DryRunResult>(
      "POST",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/rules/dry-run`" + `,
      { query, body: input, requestOptions: options },
    );
  }

  async updateRule(
    monitorId: Int64,
    ruleId: Int64,
    input: RuleUpdateInput,
    options?: RequestOptions,
  ): Promise<Rule> {
    return this.request<Rule>(
      "PATCH",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/rules/${encodeURIComponent(String(ruleId))}`" + `,
      { body: input, requestOptions: options },
    );
  }

  async deleteRule(
    monitorId: Int64,
    ruleId: Int64,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "DELETE",
      ` + "`/monitors/${encodeURIComponent(String(monitorId))}/rules/${encodeURIComponent(String(ruleId))}`" + `,
      { requestOptions: options },
    );
  }

  // ---------------------------------------------------------------------------
  // Channels & Channel Signing Keys
  // ---------------------------------------------------------------------------

  async listChannels(
    params?: ListChannelsParams,
    options?: RequestOptions,
  ): Promise<ChannelPage> {
    return this.request<ChannelPage>("GET", "/channels", {
      query: params as Record<string, unknown>,
      requestOptions: options,
    });
  }

  /**
   * Async iterator that transparently pages through GET /channels using
   * ` + "`next_cursor`" + ` until all matching channels have been yielded.
   */
  async *paginateChannels(
    params?: Omit<ListChannelsParams, "cursor">,
    options?: RequestOptions,
  ): AsyncGenerator<Channel, void, unknown> {
    let cursor: string | undefined = undefined;
    do {
      const page = await this.listChannels({ ...params, cursor }, options);
      for (const item of page.channels ?? []) {
        yield item;
      }
      cursor = page.next_cursor || undefined;
    } while (cursor);
  }

  async createChannel(
    input: ChannelInput,
    options?: RequestOptions,
  ): Promise<Channel> {
    return this.request<Channel>("POST", "/channels", {
      body: input,
      requestOptions: options,
    });
  }

  async getChannel(id: Int64, options?: RequestOptions): Promise<Channel> {
    return this.request<Channel>(
      "GET",
      ` + "`/channels/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async updateChannel(
    id: Int64,
    input: ChannelUpdateInput,
    options?: RequestOptions,
  ): Promise<Channel> {
    return this.request<Channel>(
      "PATCH",
      ` + "`/channels/${encodeURIComponent(String(id))}`" + `,
      { body: input, requestOptions: options },
    );
  }

  async deleteChannel(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "DELETE",
      ` + "`/channels/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async bulkUpdateChannels(
    input: { ids: Int64[]; enabled: boolean },
    options?: RequestOptions,
  ): Promise<{ updated: number; unknown_ids: Int64Value[]; enabled: boolean }> {
    return this.request("POST", "/channels/bulk", {
      body: input,
      requestOptions: options,
    });
  }

  async getChannelStats(
    id: Int64,
    query?: { window?: "24h" | "7d" | "30d" },
    options?: RequestOptions,
  ): Promise<ChannelDeliveryStats> {
    return this.request<ChannelDeliveryStats>(
      "GET",
      ` + "`/channels/${encodeURIComponent(String(id))}/stats`" + `,
      { query, requestOptions: options },
    );
  }

  async duplicateChannel(
    id: Int64,
    input?: { name?: string },
    options?: RequestOptions,
  ): Promise<Channel> {
    return this.request<Channel>(
      "POST",
      ` + "`/channels/${encodeURIComponent(String(id))}/duplicate`" + `,
      { body: input ?? {}, requestOptions: options },
    );
  }

  async testChannel(
    id: Int64,
    options?: RequestOptions,
  ): Promise<{ status?: string }> {
    return this.request<{ status?: string }>(
      "POST",
      ` + "`/channels/${encodeURIComponent(String(id))}/test`" + `,
      { requestOptions: options },
    );
  }

  async listChannelKeys(
    channelId: Int64,
    options?: RequestOptions,
  ): Promise<{ keys?: ChannelSigningKey[] } | ChannelSigningKey[]> {
    return this.request(
      "GET",
      ` + "`/channels/${encodeURIComponent(String(channelId))}/keys`" + `,
      { requestOptions: options },
    );
  }

  async rotateChannelKey(
    channelId: Int64,
    input?: { overlap?: string },
    options?: RequestOptions,
  ): Promise<RotatedChannelSigningKey> {
    return this.request<RotatedChannelSigningKey>(
      "POST",
      ` + "`/channels/${encodeURIComponent(String(channelId))}/keys/rotate`" + `,
      { body: input ?? {}, requestOptions: options },
    );
  }

  async retireChannelKey(
    channelId: Int64,
    keyId: string,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "POST",
      ` + "`/channels/${encodeURIComponent(String(channelId))}/keys/${encodeURIComponent(keyId)}/retire`" + `,
      { requestOptions: options },
    );
  }

  async revokeChannelKey(
    channelId: Int64,
    keyId: string,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "POST",
      ` + "`/channels/${encodeURIComponent(String(channelId))}/keys/${encodeURIComponent(keyId)}/revoke`" + `,
      { requestOptions: options },
    );
  }

  // ---------------------------------------------------------------------------
  // Maintenance Windows & Inhibitions
  // ---------------------------------------------------------------------------

  async listMaintenanceWindows(
    params?: ListMaintenanceWindowsParams,
    options?: RequestOptions,
  ): Promise<MaintenanceWindowPage> {
    return this.request<MaintenanceWindowPage>("GET", "/maintenance-windows", {
      query: params as Record<string, unknown>,
      requestOptions: options,
    });
  }

  async createMaintenanceWindow(
    input: MaintenanceWindowInput,
    options?: RequestOptions,
  ): Promise<MaintenanceWindow> {
    return this.request<MaintenanceWindow>("POST", "/maintenance-windows", {
      body: input,
      requestOptions: options,
    });
  }

  async getMaintenanceWindow(
    id: Int64,
    options?: RequestOptions,
  ): Promise<MaintenanceWindow> {
    return this.request<MaintenanceWindow>(
      "GET",
      ` + "`/maintenance-windows/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async updateMaintenanceWindow(
    id: Int64,
    input: MaintenanceWindowUpdateInput,
    options?: RequestOptions,
  ): Promise<MaintenanceWindow> {
    return this.request<MaintenanceWindow>(
      "PATCH",
      ` + "`/maintenance-windows/${encodeURIComponent(String(id))}`" + `,
      { body: input, requestOptions: options },
    );
  }

  async deleteMaintenanceWindow(
    id: Int64,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "DELETE",
      ` + "`/maintenance-windows/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async listInhibitions(
    options?: RequestOptions,
  ): Promise<{ inhibitions: Inhibition[] }> {
    return this.request<{ inhibitions: Inhibition[] }>("GET", "/inhibitions", {
      requestOptions: options,
    });
  }

  async createInhibition(
    input: InhibitionInput,
    options?: RequestOptions,
  ): Promise<Inhibition> {
    return this.request<Inhibition>("POST", "/inhibitions", {
      body: input,
      requestOptions: options,
    });
  }

  async deleteInhibition(
    sourceId: Int64,
    targetId: Int64,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "DELETE",
      ` + "`/inhibitions/${encodeURIComponent(String(sourceId))}/${encodeURIComponent(String(targetId))}`" + `,
      { requestOptions: options },
    );
  }

  // ---------------------------------------------------------------------------
  // Alerts, Streaming, Exports & Deliveries
  // ---------------------------------------------------------------------------

  async listAlerts(
    params?: ListAlertsParams,
    options?: RequestOptions,
  ): Promise<AlertPage> {
    return this.request<AlertPage>("GET", "/alerts", {
      query: params as Record<string, unknown>,
      requestOptions: options,
    });
  }

  /**
   * Async iterator that transparently pages through GET /alerts using
   * ` + "`next_cursor`" + ` until all matching alerts have been yielded.
   */
  async *paginateAlerts(
    params?: Omit<ListAlertsParams, "cursor">,
    options?: RequestOptions,
  ): AsyncGenerator<Alert, void, unknown> {
    let cursor: string | undefined = undefined;
    do {
      const page = await this.listAlerts({ ...params, cursor }, options);
      for (const item of page.alerts ?? []) {
        yield item;
      }
      cursor = page.next_cursor || undefined;
    } while (cursor);
  }

  /**
   * Connects to GET /alerts/stream (Server-Sent Events) and yields decoded
   * ` + "`Alert`" + ` objects as they arrive.
   */
  async *streamAlerts(
    params?: { monitor_id?: Int64 },
    options?: RequestOptions,
  ): AsyncGenerator<Alert, void, unknown> {
    const headers = this.buildHeaders(options?.headers, false);
    headers["Accept"] = "text/event-stream";
    const res = await this.fetchImpl(
      this.buildUrl("/alerts/stream", params as Record<string, unknown>),
      {
        method: "GET",
        headers,
        signal: options?.signal,
      },
    );
    if (!res.ok) {
      const rawText = await res.text();
      throw new SoroBeaconAPIError(res.status, {
        error: rawText || ` + "`HTTP ${res.status}`" + `,
      });
    }
    if (!res.body) {
      return;
    }
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      while (true) {
        const { value, done } = await reader.read();
        if (done) {
          break;
        }
        buffer += decoder.decode(value, { stream: true });
        let sepIdx: number;
        while ((sepIdx = buffer.indexOf("\n\n")) !== -1) {
          const chunk = buffer.slice(0, sepIdx);
          buffer = buffer.slice(sepIdx + 2);
          const dataLines: string[] = [];
          for (const line of chunk.split("\n")) {
            if (line.startsWith("data:")) {
              dataLines.push(line.slice(5).trimStart());
            }
          }
          if (dataLines.length > 0) {
            yield parseJSONPreservingWideInts<Alert>(dataLines.join("\n"));
          }
        }
      }
    } finally {
      reader.releaseLock();
    }
  }

  async exportAlertsCSV(
    params?: Omit<ListAlertsParams, "cursor">,
    options?: RequestOptions,
  ): Promise<string> {
    return this.request<string>("GET", "/alerts.csv", {
      query: params as Record<string, unknown>,
      accept: "text/csv",
      parseAsText: true,
      requestOptions: options,
    });
  }

  async exportAlertsNDJSON(
    params?: Omit<ListAlertsParams, "cursor" | "network">,
    options?: RequestOptions,
  ): Promise<Alert[]> {
    const raw = await this.request<string>("GET", "/alerts/export", {
      query: params as Record<string, unknown>,
      accept: "application/x-ndjson",
      parseAsText: true,
      requestOptions: options,
    });
    if (!raw || !raw.trim()) {
      return [];
    }
    return raw
      .trim()
      .split("\n")
      .filter((line) => line.trim().length > 0)
      .map((line) => parseJSONPreservingWideInts<Alert>(line));
  }

  async acknowledgeAlert(
    id: Int64,
    options?: RequestOptions,
  ): Promise<Alert> {
    return this.request<Alert>(
      "POST",
      ` + "`/alerts/${encodeURIComponent(String(id))}/acknowledge`" + `,
      { requestOptions: options },
    );
  }

  async listAlertDeliveries(
    alertId: Int64,
    params?: { status?: "success" | "failed" },
    options?: RequestOptions,
  ): Promise<DeliveryAttempt[]> {
    return this.request<DeliveryAttempt[]>(
      "GET",
      ` + "`/alerts/${encodeURIComponent(String(alertId))}/deliveries`" + `,
      { query: params, requestOptions: options },
    );
  }

  async retryAlertDelivery(
    alertId: Int64,
    channelId: Int64,
    options?: RequestOptions,
  ): Promise<DeliveryAttempt> {
    return this.request<DeliveryAttempt>(
      "POST",
      ` + "`/alerts/${encodeURIComponent(String(alertId))}/deliveries/${encodeURIComponent(String(channelId))}/retry`" + `,
      { requestOptions: options },
    );
  }

  // ---------------------------------------------------------------------------
  // Dead Letters, Federated Ingest & External Triggers
  // ---------------------------------------------------------------------------

  async listDeadLetters(
    params?: { limit?: number },
    options?: RequestOptions,
  ): Promise<{ dead_letters: JsonObject[] }> {
    return this.request<{ dead_letters: JsonObject[] }>("GET", "/dead-letters", {
      query: params,
      requestOptions: options,
    });
  }

  async listDeadLetterGroups(
    options?: RequestOptions,
  ): Promise<JsonObject[]> {
    return this.request<JsonObject[]>("GET", "/dead-letters/groups", {
      requestOptions: options,
    });
  }

  async bulkRedriveDeadLetters(
    input?: JsonObject,
    options?: RequestOptions,
  ): Promise<JsonObject> {
    return this.request<JsonObject>("POST", "/dead-letters/bulk/redrive", {
      body: input ?? {},
      requestOptions: options,
    });
  }

  async bulkDiscardDeadLetters(
    input?: JsonObject,
    options?: RequestOptions,
  ): Promise<JsonObject> {
    return this.request<JsonObject>("POST", "/dead-letters/bulk/discard", {
      body: input ?? {},
      requestOptions: options,
    });
  }

  async redriveDeadLetter(
    id: Int64,
    options?: RequestOptions,
  ): Promise<JsonObject> {
    return this.request<JsonObject>(
      "POST",
      ` + "`/dead-letters/${encodeURIComponent(String(id))}/redrive`" + `,
      { requestOptions: options },
    );
  }

  async discardDeadLetter(
    id: Int64,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "POST",
      ` + "`/dead-letters/${encodeURIComponent(String(id))}/discard`" + `,
      { requestOptions: options },
    );
  }

  async ingestFederatedAlert(
    input: FederatedIngestInput,
    options?: RequestOptions,
  ): Promise<FederatedIngestResult> {
    return this.request<FederatedIngestResult>("POST", "/ingest", {
      body: input,
      requestOptions: options,
    });
  }

  async triggerEvaluation(
    input: TriggerEvaluationInput,
    options?: RequestOptions,
  ): Promise<TriggerEvaluationResult> {
    return this.request<TriggerEvaluationResult>("POST", "/triggers", {
      body: input,
      requestOptions: options,
    });
  }

  // ---------------------------------------------------------------------------
  // System Health, Poller, Version, Rule Types, Stats & Audit
  // ---------------------------------------------------------------------------

  async getHealth(options?: RequestOptions): Promise<JsonObject> {
    return this.request<JsonObject>("GET", "/health", {
      requestOptions: options,
    });
  }

  async getLivez(options?: RequestOptions): Promise<JsonObject | string> {
    return this.request<JsonObject | string>("GET", "/livez", {
      requestOptions: options,
    });
  }

  async getReadyz(options?: RequestOptions): Promise<JsonObject> {
    return this.request<JsonObject>("GET", "/readyz", {
      requestOptions: options,
    });
  }

  async getPollerStatus(options?: RequestOptions): Promise<PollerStatus> {
    return this.request<PollerStatus>("GET", "/poller", {
      requestOptions: options,
    });
  }

  async getVersion(options?: RequestOptions): Promise<Version> {
    return this.request<Version>("GET", "/version", {
      requestOptions: options,
    });
  }

  async listRuleTypes(
    options?: RequestOptions,
  ): Promise<RuleTypesResponse> {
    return this.request<RuleTypesResponse>("GET", "/rule-types", {
      requestOptions: options,
    });
  }

  async getStats(options?: RequestOptions): Promise<ServiceStats> {
    return this.request<ServiceStats>("GET", "/stats", {
      requestOptions: options,
    });
  }

  async getDailyAlertCounts(
    options?: RequestOptions,
  ): Promise<DailyAlertCountsResponse> {
    return this.request<DailyAlertCountsResponse>("GET", "/stats/alerts-daily", {
      requestOptions: options,
    });
  }

  async listAuditLogs(
    params?: ListAuditParams,
    options?: RequestOptions,
  ): Promise<AuditPage> {
    return this.request<AuditPage>("GET", "/audit", {
      query: params as Record<string, unknown>,
      requestOptions: options,
    });
  }

  /**
   * Async iterator that transparently pages through GET /audit by passing the
   * last seen entry ` + "`id`" + ` as the next ` + "`cursor`" + `.
   */
  async *paginateAuditLogs(
    params?: Omit<ListAuditParams, "cursor">,
    options?: RequestOptions,
  ): AsyncGenerator<AuditEntry, void, unknown> {
    let cursor: Int64 | undefined = undefined;
    const limit = params?.limit ?? 50;
    while (true) {
      const page = await this.listAuditLogs({ ...params, limit, cursor }, options);
      const entries = page.entries ?? [];
      for (const entry of entries) {
        yield entry;
      }
      if (entries.length < limit) {
        break;
      }
      cursor = entries[entries.length - 1].id;
    }
  }

  // ---------------------------------------------------------------------------
  // Monitor Templates
  // ---------------------------------------------------------------------------

  async listTemplates(
    options?: RequestOptions,
  ): Promise<{ templates: MonitorTemplate[] }> {
    return this.request<{ templates: MonitorTemplate[] }>("GET", "/templates", {
      requestOptions: options,
    });
  }

  async createTemplate(
    input: MonitorTemplateInput,
    options?: RequestOptions,
  ): Promise<MonitorTemplate> {
    return this.request<MonitorTemplate>("POST", "/templates", {
      body: input,
      requestOptions: options,
    });
  }

  async getTemplate(
    id: Int64,
    options?: RequestOptions,
  ): Promise<MonitorTemplate> {
    return this.request<MonitorTemplate>(
      "GET",
      ` + "`/templates/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async updateTemplate(
    id: Int64,
    input: MonitorTemplateUpdateInput,
    options?: RequestOptions,
  ): Promise<MonitorTemplate> {
    return this.request<MonitorTemplate>(
      "PATCH",
      ` + "`/templates/${encodeURIComponent(String(id))}`" + `,
      { body: input, requestOptions: options },
    );
  }

  async deleteTemplate(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "DELETE",
      ` + "`/templates/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async instantiateTemplate(
    id: Int64,
    input: InstantiateInput,
    options?: RequestOptions,
  ): Promise<Monitor> {
    return this.request<Monitor>(
      "POST",
      ` + "`/templates/${encodeURIComponent(String(id))}/instantiate`" + `,
      { body: input, requestOptions: options },
    );
  }

  async bulkInstantiateTemplate(
    id: Int64,
    instances: InstantiateInput[],
    options?: RequestOptions,
  ): Promise<{ created: { name: string; id: Int64Value }[] }> {
    return this.request<{ created: { name: string; id: Int64Value }[] }>(
      "POST",
      ` + "`/templates/${encodeURIComponent(String(id))}/instantiate/bulk`" + `,
      { body: { instances }, requestOptions: options },
    );
  }

  // ---------------------------------------------------------------------------
  // Scoped API Tokens & Persisted GraphQL Operations
  // ---------------------------------------------------------------------------

  async listTokens(
    options?: RequestOptions,
  ): Promise<{ tokens: APIToken[] }> {
    return this.request<{ tokens: APIToken[] }>("GET", "/tokens", {
      requestOptions: options,
    });
  }

  async createToken(
    input: TokenInput,
    options?: RequestOptions,
  ): Promise<CreatedAPIToken> {
    return this.request<CreatedAPIToken>("POST", "/tokens", {
      body: input,
      requestOptions: options,
    });
  }

  async getToken(id: Int64, options?: RequestOptions): Promise<APIToken> {
    return this.request<APIToken>(
      "GET",
      ` + "`/tokens/${encodeURIComponent(String(id))}`" + `,
      { requestOptions: options },
    );
  }

  async revokeToken(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "POST",
      ` + "`/tokens/${encodeURIComponent(String(id))}/revoke`" + `,
      { requestOptions: options },
    );
  }

  async rotateToken(
    id: Int64,
    input?: RotateTokenInput,
    options?: RequestOptions,
  ): Promise<CreatedAPIToken> {
    return this.request<CreatedAPIToken>(
      "POST",
      ` + "`/tokens/${encodeURIComponent(String(id))}/rotate`" + `,
      { body: input ?? {}, requestOptions: options },
    );
  }

  async abandonTokenRotation(
    id: Int64,
    options?: RequestOptions,
  ): Promise<APIToken | void> {
    return this.request<APIToken | void>(
      "POST",
      ` + "`/tokens/${encodeURIComponent(String(id))}/abandon`" + `,
      { requestOptions: options },
    );
  }

  async listGraphQLOperations(
    options?: RequestOptions,
  ): Promise<{ operations?: PersistedGraphQLOperation[] } | PersistedGraphQLOperation[]> {
    return this.request("GET", "/graphql/operations", {
      requestOptions: options,
    });
  }

  async registerGraphQLOperation(
    input: { id?: string; name?: string; query: string },
    options?: RequestOptions,
  ): Promise<PersistedGraphQLOperation> {
    return this.request<PersistedGraphQLOperation>("POST", "/graphql/operations", {
      body: input,
      requestOptions: options,
    });
  }

  async getGraphQLOperation(
    id: string,
    options?: RequestOptions,
  ): Promise<PersistedGraphQLOperation> {
    return this.request<PersistedGraphQLOperation>(
      "GET",
      ` + "`/graphql/operations/${encodeURIComponent(id)}`" + `,
      { requestOptions: options },
    );
  }

  async deleteGraphQLOperation(
    id: string,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "DELETE",
      ` + "`/graphql/operations/${encodeURIComponent(id)}`" + `,
      { requestOptions: options },
    );
  }
}
`)
}

func joinQuoted(items []string, sep string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, sep)
}

func writeDocComment(b *bytes.Buffer, desc, indent string) {
	lines := strings.Split(strings.TrimSpace(desc), "\n")
	if len(lines) == 1 && len(lines[0]) <= 76 {
		fmt.Fprintf(b, "%s/** %s */\n", indent, strings.ReplaceAll(lines[0], "*/", "* /"))
		return
	}
	fmt.Fprintf(b, "%s/**\n", indent)
	for _, line := range lines {
		clean := strings.TrimSpace(strings.ReplaceAll(line, "*/", "* /"))
		if clean == "" {
			fmt.Fprintf(b, "%s *\n", indent)
		} else {
			fmt.Fprintf(b, "%s * %s\n", indent, clean)
		}
	}
	fmt.Fprintf(b, "%s */\n", indent)
}
