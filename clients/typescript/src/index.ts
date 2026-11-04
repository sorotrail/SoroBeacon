// Code generated from internal/api/openapi.json (SoroBeacon API v1.0.0). DO NOT EDIT.
// Regenerate with: go generate ./clients/typescript/...

/**
 * 64-bit integer input value (resource IDs, ledger sequence numbers, counters,
 * and wide Soroban integers). Callers may pass a safe JavaScript number, a
 * bigint, or a base-10 integer string so values above Number.MAX_SAFE_INTEGER
 * (2^53 - 1) never lose precision.
 */
export type Int64 = number | bigint | `${number}`;

/**
 * 64-bit integer value decoded from JSON responses. Values within IEEE-754 safe
 * integer bounds are decoded as `number`; values outside
 * [Number.MIN_SAFE_INTEGER, Number.MAX_SAFE_INTEGER] are preserved losslessly
 * as decimal strings (`"9007199254740993"`) by `parseJSONPreservingWideInts`.
 */
export type Int64Value = number | `${number}`;

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

/** Stable machine-readable error codes returned in the API Error envelope. */
export type ErrorCode =
  | "NOT_FOUND"
  | "ALREADY_EXISTS"
  | "INVALID_ARGUMENT"
  | "CONFLICT"
  | "UNAUTHENTICATED"
  | "PERMISSION_DENIED"
  | "RATE_LIMITED"
  | "QUOTA_EXCEEDED"
  | "PAYLOAD_TOO_LARGE"
  | "METHOD_NOT_ALLOWED"
  | "NOT_IMPLEMENTED"
  | "SERVICE_UNAVAILABLE"
  | "INTERNAL_ERROR";

/** Built-in rule evaluation types. */
export type RuleTypeName =
  | "event_emitted"
  | "value_threshold"
  | "token_event"
  | "self_transfer"
  | "time_window"
  | "absence_of_event";

/** Supported notification delivery channel types. */
export type ChannelType =
  | "discord"
  | "slack"
  | "telegram"
  | "email"
  | "webhook"
  | "ntfy";

export interface APIToken {
  created_at: ISODateTimeString;
  expires_at?: ISODateTimeString | null;
  id: Int64Value;
  last_used_at?: ISODateTimeString | null;
  name: string;
  prefix: string;
  revoked_at?: ISODateTimeString | null;
  rotated_at?: ISODateTimeString | null;
  rotated_from_id?: Int64Value;
  scopes: string[];
  successor_id?: Int64Value;
  /**
   * Redacted on read: the plaintext token secret is returned only once in
   * CreatedAPIToken when minting or rotating a token.
   */
  token?: never;
}

export interface Alert {
  created_at: ISODateTimeString;
  event_id: string;
  event_name: string;
  /**
   * True when another SoroBeacon instance forwarded this alert. A federated alert belongs to no local monitor and no local rule, so its monitor_id and rule_id are zero.
   */
  federated?: boolean;
  /** The instance that forwarded the alert. Empty for a local one. */
  federation_origin?: string;
  id: Int64Value;
  monitor_id: Int64Value;
  monitor_name?: string;
  /**
   * Stellar chain the matching event came from. Taken from the monitor, never from the caller: it is what a reorg on that chain may retract and no other chain's.
   */
  network?: string;
  payload: AlertPayload;
  rule_id: Int64Value;
  /** The monitor id on the sending instance. Never a local key. */
  source_monitor_id?: Int64Value;
  /** The rule id on the sending instance. Never a local key. */
  source_rule_id?: Int64Value;
  /** True when a maintenance window silenced delivery */
  suppressed?: boolean;
  suppression_reason?: string;
}

export interface AlertPage {
  alerts: Alert[];
  /** Absent on the last page */
  next_cursor?: string;
}

export interface BulkMonitorInput {
  enabled: boolean;
  ids: Int64[];
}

export interface BulkMonitorResult {
  enabled: boolean;
  unknown_ids: Int64Value[];
  /** How many existing monitors changed state */
  updated: number;
}

export interface Channel {
  /**
   * Deliveries that have failed since the last success. One success resets it to 0.
   */
  consecutive_failures?: Int64Value;
  /**
   * Permanent failures (401/403/404) since the last success. This is what auto-disable counts; a transient 5xx or timeout neither increments nor clears it.
   */
  consecutive_permanent_failures?: Int64Value;
  /**
   * Set when health tracking auto-disabled the channel, absent when it is on or an operator disabled it. Cleared only by explicitly re-enabling the channel.
   */
  disabled_at?: ISODateTimeString;
  enabled: boolean;
  id: Int64Value;
  /**
   * Most recent failure message, empty once a delivery succeeds. Never contains channel config.
   */
  last_error?: string;
  /**
   * When last_error was recorded. Absent if there has been no failure since the last success.
   */
  last_error_at?: ISODateTimeString;
  /** Last delivery that succeeded. Absent until the first one. */
  last_success_at?: ISODateTimeString;
  name: string;
  /** HTTP delivery timeout in seconds (default: 15, range 1-60) */
  timeout?: number;
  type: ChannelType;
  /**
   * Redacted on read: channel configuration contains secrets (webhook URLs,
   * bot tokens, SMTP credentials) and is accepted only on ChannelInput.
   */
  config?: never;
}

/**
 * One channel's delivery outcomes over the requested window. Attempts are counted, not unique alerts: a retry is another attempt.
 */
export interface ChannelDeliveryStats {
  channel_id: Int64Value;
  failed: Int64Value;
  /** Null when the window holds no failed attempt */
  last_failure_at?: ISODateTimeString | null;
  /** Null when the window holds no successful attempt */
  last_success_at?: ISODateTimeString | null;
  succeeded: Int64Value;
  /**
   * succeeded / total as a ratio in [0, 1]. Omitted when there were no attempts in the window — an undefined rate is not 0 or 1.
   */
  success_rate?: number;
  /** Every delivery attempt inside the window, successful or not */
  total: Int64Value;
  /**
   * The window this response covers, as requested ("24h" when the parameter was omitted)
   */
  window: string;
}

export interface ChannelInput {
  /** Type-specific; the channel reference documents each */
  config: JsonObject;
  /**
   * Empty delivers immediately; window batches into one summary per digest_window_seconds
   */
  digest_mode?: "" | "window";
  /** Accumulation window when digest_mode is window; must be > 0 then */
  digest_window_seconds?: Int64;
  enabled?: boolean;
  name: string;
  /** HTTP delivery timeout in seconds (default: 15, range 1-60) */
  timeout?: number;
  type: ChannelType;
}

export interface ChannelPage {
  channels: Channel[];
  /** Absent on the last page */
  next_cursor?: string;
}

export interface CreatedAPIToken {
  created_at: ISODateTimeString;
  expires_at?: ISODateTimeString | null;
  id: Int64Value;
  last_used_at?: ISODateTimeString | null;
  name: string;
  prefix: string;
  revoked_at?: ISODateTimeString | null;
  rotated_at?: ISODateTimeString | null;
  rotated_from_id?: Int64Value;
  scopes: string[];
  successor_id?: Int64Value;
  /** Plaintext token secret, returned only once on creation or rotation */
  token: string;
}

export interface DeliveryAttempt {
  alert_id: Int64Value;
  attempted_at: ISODateTimeString;
  channel_id: Int64Value;
  response_snippet?: string;
  status: "success" | "failed";
}

export interface DryRunResult {
  /** Number of historical events that matched the rule */
  count: number;
  /** Sample of matching events */
  matches: { contract_id?: string; event_id?: string; event_name?: string; ledger?: number }[];
  /** Corpus bias and window documentation */
  note: string;
  /** Total number of historical events evaluated */
  total_evaluated: number;
}

export interface Error {
  /** Stable machine-readable error code */
  code?: ErrorCode;
  /**
   * Field-level validation problems. Omitted when there are none so existing clients keep working. Each entry names a JSON path and why it failed; all problems in one request are listed together.
   */
  details?: ErrorDetail[];
  /** Human-readable message */
  error: string;
  /** Correlation ID, also on the X-Request-ID response header */
  request_id?: string;
}

export interface ErrorDetail {
  /** Dotted JSON path of the offending field, e.g. rules[0].params.min_amount */
  field: string;
  /** Human-readable failure for that field */
  reason: string;
}

export interface EscalationInput {
  /**
   * Ordered escalation steps. The first fires immediately; each later step waits its delay after the previous one fired.
   */
  steps: { channel_ids: Int64[]; delay?: string }[];
}

export interface EscalationPolicy {
  created_at: ISODateTimeString;
  id: Int64Value;
  monitor_id: Int64Value;
  steps: EscalationStep[];
  updated_at: ISODateTimeString;
}

export interface EscalationStep {
  channel_ids: Int64Value[];
  /** Go duration before this step fires, e.g. 5m */
  delay: string;
  /** 0-based place in the policy */
  position: number;
}

export interface ImportInput {
  channel_ids?: Int64[];
  contract_ids: string[];
  enabled?: boolean;
  /** Monitor name prefix */
  name: string;
  /**
   * Chain every monitor in this batch watches. Omitted means the primary. The already-monitored check is per network: the same id on another chain is a different contract, and watching it there is normal.
   */
  network?: string;
}

export interface ImportRowResult {
  contract_id?: string;
  error?: string;
  monitor_id?: Int64Value;
  row?: number;
  status?: "created" | "skipped" | "error";
}

export interface Inhibition {
  created_at: ISODateTimeString;
  /** Source counts as firing with an alert inside this window */
  firing_window_seconds: number;
  source_rule_id: Int64Value;
  target_rule_id: Int64Value;
}

export interface InhibitionInput {
  /** Optional; defaults to 300 when absent or non-positive */
  firing_window_seconds?: number;
  source_rule_id: Int64;
  target_rule_id: Int64;
}

export interface InstantiateInput {
  contract_ids: string[];
  name: string;
  /**
   * Chain the instantiated monitor watches. A template is chain-agnostic text, so this defaults to the instance's primary network.
   */
  network?: string;
  parameters?: Record<string, string>;
}

export interface MaintenanceWindow {
  contract_id?: string;
  created_at: ISODateTimeString;
  end_at: ISODateTimeString;
  id: Int64Value;
  monitor_id?: Int64Value;
  reason: string;
  scope: "global" | "monitor" | "contract";
  start_at: ISODateTimeString;
}

export interface MaintenanceWindowInput {
  /** Required when scope is contract */
  contract_id?: string;
  /** Must be after start_at; open-ended windows are rejected */
  end_at: ISODateTimeString;
  /** Required when scope is monitor */
  monitor_id?: Int64;
  reason: string;
  scope: "global" | "monitor" | "contract";
  start_at: ISODateTimeString;
}

export interface MaintenanceWindowPage {
  maintenance_windows: MaintenanceWindow[];
}

export interface Monitor {
  channel_ids?: Int64Value[];
  /** Soroban contract strkeys (C...) */
  contract_ids: string[];
  enabled: boolean;
  id: Int64Value;
  name: string;
  /**
   * Stellar chain this monitor watches. Empty on rows created before multi-network ingestion, which startup labels with the primary network.
   */
  network?: string;
  /**
   * Free-form labels for grouping and filtering, normalised to trimmed lowercase unique strings before storage. Always an array; empty when the monitor carries no tags.
   */
  tags?: string[];
}

export interface MonitorInput {
  channel_ids?: Int64[];
  contract_ids: string[];
  enabled?: boolean;
  name: string;
  /**
   * Stellar chain these contract ids live on, chosen at creation. Omitted means the instance's primary network (the first entry in NETWORKS, or NETWORK when unset). A name this instance does not poll is rejected: the monitor would match nothing forever. A monitor's network cannot be changed afterwards — a contract id resolves to a different contract on another chain, and the alerts already stored keep their old label — so moving one means deleting and recreating it.
   */
  network?: string;
  /**
   * Free-form labels for grouping and filtering. Values are trimmed, lowercased and deduplicated before storage, so "Treasury" and "treasury" are one tag. Omitted leaves the set unchanged on update and empty on create.
   */
  tags?: string[];
}

export interface MonitorPage {
  monitors: Monitor[];
  /** Absent on the last page */
  next_cursor?: string;
}

export interface MonitorStats {
  alerts: Int64Value;
  alerts_last_24h: Int64Value;
  alerts_last_7d: Int64Value;
  deliveries_failed: Int64Value;
  deliveries_succeeded: Int64Value;
  /** Absent when the monitor has never alerted */
  last_alert_at?: ISODateTimeString;
  monitor_id: Int64Value;
  /** Every rule of the monitor, matched or not */
  rules: RuleMatchCount[];
}

export interface MonitorTemplate {
  channel_ids?: Int64Value[];
  created_at: ISODateTimeString;
  description?: string;
  id: Int64Value;
  name: string;
  parameters?: { default?: string; description?: string; name?: string; required?: boolean }[];
  rules?: { params?: JsonObject; type?: string }[];
}

export interface MonitorTemplateInput {
  channel_ids?: Int64[];
  description?: string;
  name: string;
  parameters?: { default?: string; description?: string; name?: string; required?: boolean }[];
  rules?: { params?: JsonObject; type?: string }[];
}

export interface PollerStatus {
  backing_off: boolean;
  last_processed_ledger?: Int64Value;
  last_successful_poll?: ISODateTimeString;
  latest_chain_ledger?: Int64Value;
  ledger_lag?: Int64Value;
  status: "waiting" | "ok";
}

export interface Rule {
  enabled: boolean;
  id: Int64Value;
  monitor_id: Int64Value;
  params: JsonObject;
  type: RuleTypeName;
}

export interface RuleInput {
  enabled?: boolean;
  /** Type-specific; validated against the rule type */
  params: JsonObject;
  type: RuleTypeName;
}

export interface RuleMatchCount {
  alerts?: Int64Value;
  rule_id?: Int64Value;
  type?: string;
}

export interface RuleParameter {
  default: string;
  description: string;
  name: string;
  options?: string[];
  required: boolean;
  type: "string" | "number" | "object" | "select";
}

export interface RuleType {
  parameters: RuleParameter[];
  type: string;
}

export interface RuleTypesResponse {
  rule_types: RuleType[];
}

export interface TokenInput {
  /** Go duration string (e.g. 168h); omit for a token that never expires */
  expires_in?: string;
  name: string;
  scopes: string[];
}

export interface Version {
  build_date: string;
  commit: string;
  version: string;
}

/** Partial update payload for PATCH /monitors/{id}. */
export type MonitorUpdateInput = Partial<MonitorInput>;

/** Partial update payload for PATCH /rules/{ruleID}. */
export type RuleUpdateInput = Partial<RuleInput>;

/**
 * Partial update payload for PATCH /channels/{id}.
 * `config` is accepted on write when rotating or updating channel credentials,
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

/** Query and header parameters for DELETE /workspaces/{id}. */
export interface DeleteWorkspaceParams {
  dry_run?: boolean;
  confirm?: string;
  batch_size?: number;
  max_alert_batches?: number;
  confirm_header?: string;
}

/** Request payload and header parameters for POST /workspaces/{id}/delete. */
export interface PostDeleteWorkspaceInput {
  dry_run?: boolean;
  confirm?: string;
  batch_size?: number;
  max_alert_batches?: number;
  confirm_header?: string;
}

/**
 * Converts an Int64 value (number, bigint, or decimal string) into a native
 * `bigint` without IEEE-754 precision loss.
 */
export function toBigInt(value: Int64): bigint {
  if (typeof value === "bigint") {
    return value;
  }
  if (typeof value === "number") {
    if (!Number.isInteger(value) || !Number.isSafeInteger(value)) {
      throw new RangeError(
        `unsafe integer number ${value}; pass a bigint or decimal string instead`,
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
 * Serializes a request body to JSON, emitting `bigint` values as exact
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
 * Exposes the HTTP status, machine-readable `code` from the Error taxonomy,
 * correlation `requestId`, and field-level validation `details`.
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
    super(envelope.error || `HTTP ${status}`);
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

/** Complete table of OpenAPI operations covered by SoroBeaconClient. */
export const OPENAPI_OPERATIONS = [
  { method: "GET", path: "/monitors", clientMethod: "listMonitors", summary: "List monitors" },
  { method: "POST", path: "/monitors", clientMethod: "createMonitor", summary: "Create a monitor" },
  { method: "POST", path: "/monitors/bulk", clientMethod: "bulkUpdateMonitors", summary: "Bulk enable or disable monitors" },
  { method: "POST", path: "/monitors/import", clientMethod: "importMonitors", summary: "Bulk import contracts from a file" },
  { method: "GET", path: "/monitors/trash", clientMethod: "listTrashMonitors", summary: "List deleted monitors (trash)" },
  { method: "GET", path: "/monitors/{id}", clientMethod: "getMonitor", summary: "Fetch one monitor" },
  { method: "PATCH", path: "/monitors/{id}", clientMethod: "updateMonitor", summary: "Update a monitor" },
  { method: "DELETE", path: "/monitors/{id}", clientMethod: "deleteMonitor", summary: "Delete a monitor" },
  { method: "POST", path: "/monitors/{id}/soft-delete", clientMethod: "softDeleteMonitor", summary: "Soft-delete a monitor" },
  { method: "POST", path: "/monitors/{id}/restore", clientMethod: "restoreMonitor", summary: "Restore a soft-deleted monitor" },
  { method: "POST", path: "/monitors/{id}/duplicate", clientMethod: "duplicateMonitor", summary: "Duplicate a monitor" },
  { method: "GET", path: "/monitors/{id}/stats", clientMethod: "getMonitorStats", summary: "Per-monitor statistics" },
  { method: "GET", path: "/monitors/{id}/escalation", clientMethod: "getMonitorEscalation", summary: "Get a monitor's escalation policy" },
  { method: "PUT", path: "/monitors/{id}/escalation", clientMethod: "setMonitorEscalation", summary: "Create or replace a monitor's escalation policy" },
  { method: "DELETE", path: "/monitors/{id}/escalation", clientMethod: "deleteMonitorEscalation", summary: "Delete a monitor's escalation policy" },
  { method: "GET", path: "/monitors/{id}/rules", clientMethod: "listRules", summary: "List a monitor's rules" },
  { method: "POST", path: "/monitors/{id}/rules", clientMethod: "createRule", summary: "Add a rule to a monitor" },
  { method: "POST", path: "/monitors/{id}/rules/bulk", clientMethod: "bulkCreateRules", summary: "Create several rules in one request" },
  { method: "POST", path: "/monitors/{id}/rules/dry-run", clientMethod: "dryRunRule", summary: "Dry-run a rule against historical events" },
  { method: "PATCH", path: "/monitors/{id}/rules/{ruleID}", clientMethod: "updateRule", summary: "Update a rule" },
  { method: "DELETE", path: "/monitors/{id}/rules/{ruleID}", clientMethod: "deleteRule", summary: "Delete a rule" },
  { method: "GET", path: "/channels", clientMethod: "listChannels", summary: "List channels" },
  { method: "POST", path: "/channels", clientMethod: "createChannel", summary: "Create a notification channel" },
  { method: "POST", path: "/channels/bulk", clientMethod: "bulkUpdateChannels", summary: "Bulk enable or disable channels" },
  { method: "GET", path: "/channels/{id}", clientMethod: "getChannel", summary: "Fetch one channel" },
  { method: "PATCH", path: "/channels/{id}", clientMethod: "updateChannel", summary: "Update a channel" },
  { method: "DELETE", path: "/channels/{id}", clientMethod: "deleteChannel", summary: "Delete a channel" },
  { method: "GET", path: "/channels/{id}/stats", clientMethod: "getChannelStats", summary: "Per-channel delivery statistics" },
  { method: "POST", path: "/channels/{id}/duplicate", clientMethod: "duplicateChannel", summary: "Duplicate a channel" },
  { method: "POST", path: "/channels/{id}/test", clientMethod: "testChannel", summary: "Send a test notification through a channel" },
  { method: "GET", path: "/channels/{id}/keys", clientMethod: "listChannelKeys", summary: "List signing keys for a channel" },
  { method: "POST", path: "/channels/{id}/keys/rotate", clientMethod: "rotateChannelKey", summary: "Rotate channel signing key with overlap" },
  { method: "POST", path: "/channels/{id}/keys/{keyID}/retire", clientMethod: "retireChannelKey", summary: "Retire a channel signing key" },
  { method: "POST", path: "/channels/{id}/keys/{keyID}/revoke", clientMethod: "revokeChannelKey", summary: "Revoke a channel signing key immediately" },
  { method: "GET", path: "/maintenance-windows", clientMethod: "listMaintenanceWindows", summary: "List maintenance windows" },
  { method: "POST", path: "/maintenance-windows", clientMethod: "createMaintenanceWindow", summary: "Create a maintenance window" },
  { method: "GET", path: "/maintenance-windows/{id}", clientMethod: "getMaintenanceWindow", summary: "Fetch one maintenance window" },
  { method: "PATCH", path: "/maintenance-windows/{id}", clientMethod: "updateMaintenanceWindow", summary: "Update a maintenance window" },
  { method: "DELETE", path: "/maintenance-windows/{id}", clientMethod: "deleteMaintenanceWindow", summary: "Delete a maintenance window" },
  { method: "GET", path: "/inhibitions", clientMethod: "listInhibitions", summary: "List inhibition rules" },
  { method: "POST", path: "/inhibitions", clientMethod: "createInhibition", summary: "Create an inhibition rule" },
  { method: "DELETE", path: "/inhibitions/{sourceID}/{targetID}", clientMethod: "deleteInhibition", summary: "Delete an inhibition rule" },
  { method: "GET", path: "/alerts", clientMethod: "listAlerts", summary: "List alerts" },
  { method: "GET", path: "/alerts/stream", clientMethod: "streamAlerts", summary: "Stream alerts as they are created (Server-Sent Events)" },
  { method: "GET", path: "/alerts.csv", clientMethod: "exportAlertsCSV", summary: "Export alerts as CSV" },
  { method: "GET", path: "/alerts/export", clientMethod: "exportAlertsNDJSON", summary: "Export alerts as NDJSON" },
  { method: "POST", path: "/alerts/{id}/acknowledge", clientMethod: "acknowledgeAlert", summary: "Acknowledge an alert" },
  { method: "GET", path: "/alerts/{id}/deliveries", clientMethod: "listAlertDeliveries", summary: "List one alert's delivery attempts" },
  { method: "POST", path: "/alerts/{id}/deliveries/{channelID}/retry", clientMethod: "retryAlertDelivery", summary: "Retry a failed delivery" },
  { method: "GET", path: "/dead-letters", clientMethod: "listDeadLetters", summary: "List dead-letter queue records" },
  { method: "GET", path: "/dead-letters/groups", clientMethod: "listDeadLetterGroups", summary: "Roll up dead letters into groups by channel and cause" },
  { method: "POST", path: "/dead-letters/bulk/redrive", clientMethod: "bulkRedriveDeadLetters", summary: "Re-drive a batch of dead letters matching filter or IDs" },
  { method: "POST", path: "/dead-letters/bulk/discard", clientMethod: "bulkDiscardDeadLetters", summary: "Discard a batch of dead letters matching filter or IDs" },
  { method: "POST", path: "/dead-letters/{id}/redrive", clientMethod: "redriveDeadLetter", summary: "Re-drive a dead-letter delivery" },
  { method: "POST", path: "/dead-letters/{id}/discard", clientMethod: "discardDeadLetter", summary: "Discard one dead letter from the backlog" },
  { method: "POST", path: "/ingest", clientMethod: "ingestFederatedAlert", summary: "Receive a federated alert from another SoroBeacon instance" },
  { method: "POST", path: "/triggers", clientMethod: "triggerEvaluation", summary: "Trigger alert evaluation from an external event" },
  { method: "GET", path: "/health", clientMethod: "getHealth", summary: "Combined health report" },
  { method: "GET", path: "/livez", clientMethod: "getLivez", summary: "Liveness probe" },
  { method: "GET", path: "/readyz", clientMethod: "getReadyz", summary: "Readiness probe" },
  { method: "GET", path: "/poller", clientMethod: "getPollerStatus", summary: "Poller position and lag" },
  { method: "GET", path: "/version", clientMethod: "getVersion", summary: "Build information" },
  { method: "GET", path: "/rule-types", clientMethod: "listRuleTypes", summary: "List rule types and parameter schemas" },
  { method: "GET", path: "/stats", clientMethod: "getStats", summary: "Service statistics" },
  { method: "GET", path: "/stats/alerts-daily", clientMethod: "getDailyAlertCounts", summary: "Daily alert counts for the last 30 UTC days" },
  { method: "GET", path: "/usage", clientMethod: "getUsage", summary: "Per-workspace usage metering summary" },
  { method: "GET", path: "/quotas", clientMethod: "getQuotas", summary: "Per-workspace resource quotas and query cost budget status" },
  { method: "PUT", path: "/quotas", clientMethod: "setQuotas", summary: "Configure per-workspace resource quotas and query cost budget" },
  { method: "DELETE", path: "/workspaces/{id}", clientMethod: "deleteWorkspace", summary: "Delete a workspace and everything belonging to it (or preview via dry-run)" },
  { method: "POST", path: "/workspaces/{id}/delete", clientMethod: "postDeleteWorkspace", summary: "Delete a workspace and everything belonging to it (or preview via dry-run)" },
  { method: "GET", path: "/workspaces/{id}/delete", clientMethod: "getWorkspaceDeletion", summary: "Inspect workspace deletion progress or completion tombstone" },
  { method: "GET", path: "/audit", clientMethod: "listAuditLogs", summary: "Audit log of configuration changes" },
  { method: "GET", path: "/templates", clientMethod: "listTemplates", summary: "List monitor templates" },
  { method: "POST", path: "/templates", clientMethod: "createTemplate", summary: "Create a monitor template" },
  { method: "GET", path: "/templates/{id}", clientMethod: "getTemplate", summary: "Get a monitor template" },
  { method: "PATCH", path: "/templates/{id}", clientMethod: "updateTemplate", summary: "Update a monitor template" },
  { method: "DELETE", path: "/templates/{id}", clientMethod: "deleteTemplate", summary: "Delete a monitor template" },
  { method: "POST", path: "/templates/{id}/instantiate", clientMethod: "instantiateTemplate", summary: "Create a monitor from a template" },
  { method: "POST", path: "/templates/{id}/instantiate/bulk", clientMethod: "bulkInstantiateTemplate", summary: "Bulk create monitors from a template" },
  { method: "GET", path: "/tokens", clientMethod: "listTokens", summary: "List API tokens" },
  { method: "POST", path: "/tokens", clientMethod: "createToken", summary: "Mint a scoped API token" },
  { method: "GET", path: "/tokens/{id}", clientMethod: "getToken", summary: "Get an API token by ID" },
  { method: "POST", path: "/tokens/{id}/revoke", clientMethod: "revokeToken", summary: "Revoke an API token" },
  { method: "POST", path: "/tokens/{id}/rotate", clientMethod: "rotateToken", summary: "Rotate an API token with overlap" },
  { method: "POST", path: "/tokens/{id}/abandon", clientMethod: "abandonTokenRotation", summary: "Abandon an in-progress token rotation" },
  { method: "GET", path: "/graphql/operations", clientMethod: "listGraphQLOperations", summary: "List registered persisted GraphQL operations" },
  { method: "POST", path: "/graphql/operations", clientMethod: "registerGraphQLOperation", summary: "Register a persisted GraphQL operation" },
  { method: "GET", path: "/graphql/operations/{id}", clientMethod: "getGraphQLOperation", summary: "Get a registered persisted GraphQL operation" },
  { method: "DELETE", path: "/graphql/operations/{id}", clientMethod: "deleteGraphQLOperation", summary: "Delete a registered persisted GraphQL operation" },
] as const;

/** Configuration options for SoroBeaconClient. */
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
    this.baseUrl = trimmed.endsWith("/api/v1") ? trimmed : `${trimmed}/api/v1`;
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
      headers["Authorization"] = `Bearer ${this.token}`;
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
      } = { error: rawText || res.statusText || `HTTP ${res.status}` };
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
   * `next_cursor` until all matching monitors have been yielded.
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
      `/monitors/${encodeURIComponent(String(id))}`,
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
      `/monitors/${encodeURIComponent(String(id))}`,
      { body: input, requestOptions: options },
    );
  }

  async deleteMonitor(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "DELETE",
      `/monitors/${encodeURIComponent(String(id))}`,
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
      `/monitors/${encodeURIComponent(String(id))}/soft-delete`,
      { requestOptions: options },
    );
  }

  async restoreMonitor(id: Int64, options?: RequestOptions): Promise<Monitor> {
    return this.request<Monitor>(
      "POST",
      `/monitors/${encodeURIComponent(String(id))}/restore`,
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
      `/monitors/${encodeURIComponent(String(id))}/duplicate`,
      { body: input ?? {}, requestOptions: options },
    );
  }

  async getMonitorStats(
    id: Int64,
    options?: RequestOptions,
  ): Promise<MonitorStats> {
    return this.request<MonitorStats>(
      "GET",
      `/monitors/${encodeURIComponent(String(id))}/stats`,
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
      `/monitors/${encodeURIComponent(String(monitorId))}/escalation`,
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
      `/monitors/${encodeURIComponent(String(monitorId))}/escalation`,
      { body: input, requestOptions: options },
    );
  }

  async deleteMonitorEscalation(
    monitorId: Int64,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "DELETE",
      `/monitors/${encodeURIComponent(String(monitorId))}/escalation`,
      { requestOptions: options },
    );
  }

  async listRules(
    monitorId: Int64,
    options?: RequestOptions,
  ): Promise<Rule[]> {
    return this.request<Rule[]>(
      "GET",
      `/monitors/${encodeURIComponent(String(monitorId))}/rules`,
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
      `/monitors/${encodeURIComponent(String(monitorId))}/rules`,
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
      `/monitors/${encodeURIComponent(String(monitorId))}/rules/bulk`,
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
      `/monitors/${encodeURIComponent(String(monitorId))}/rules/dry-run`,
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
      `/monitors/${encodeURIComponent(String(monitorId))}/rules/${encodeURIComponent(String(ruleId))}`,
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
      `/monitors/${encodeURIComponent(String(monitorId))}/rules/${encodeURIComponent(String(ruleId))}`,
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
   * `next_cursor` until all matching channels have been yielded.
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
      `/channels/${encodeURIComponent(String(id))}`,
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
      `/channels/${encodeURIComponent(String(id))}`,
      { body: input, requestOptions: options },
    );
  }

  async deleteChannel(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "DELETE",
      `/channels/${encodeURIComponent(String(id))}`,
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
      `/channels/${encodeURIComponent(String(id))}/stats`,
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
      `/channels/${encodeURIComponent(String(id))}/duplicate`,
      { body: input ?? {}, requestOptions: options },
    );
  }

  async testChannel(
    id: Int64,
    options?: RequestOptions,
  ): Promise<{ status?: string }> {
    return this.request<{ status?: string }>(
      "POST",
      `/channels/${encodeURIComponent(String(id))}/test`,
      { requestOptions: options },
    );
  }

  async listChannelKeys(
    channelId: Int64,
    options?: RequestOptions,
  ): Promise<{ keys?: ChannelSigningKey[] } | ChannelSigningKey[]> {
    return this.request(
      "GET",
      `/channels/${encodeURIComponent(String(channelId))}/keys`,
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
      `/channels/${encodeURIComponent(String(channelId))}/keys/rotate`,
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
      `/channels/${encodeURIComponent(String(channelId))}/keys/${encodeURIComponent(keyId)}/retire`,
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
      `/channels/${encodeURIComponent(String(channelId))}/keys/${encodeURIComponent(keyId)}/revoke`,
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
      `/maintenance-windows/${encodeURIComponent(String(id))}`,
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
      `/maintenance-windows/${encodeURIComponent(String(id))}`,
      { body: input, requestOptions: options },
    );
  }

  async deleteMaintenanceWindow(
    id: Int64,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "DELETE",
      `/maintenance-windows/${encodeURIComponent(String(id))}`,
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
      `/inhibitions/${encodeURIComponent(String(sourceId))}/${encodeURIComponent(String(targetId))}`,
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
   * `next_cursor` until all matching alerts have been yielded.
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
   * `Alert` objects as they arrive.
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
        error: rawText || `HTTP ${res.status}`,
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
      `/alerts/${encodeURIComponent(String(id))}/acknowledge`,
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
      `/alerts/${encodeURIComponent(String(alertId))}/deliveries`,
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
      `/alerts/${encodeURIComponent(String(alertId))}/deliveries/${encodeURIComponent(String(channelId))}/retry`,
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
      `/dead-letters/${encodeURIComponent(String(id))}/redrive`,
      { requestOptions: options },
    );
  }

  async discardDeadLetter(
    id: Int64,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "POST",
      `/dead-letters/${encodeURIComponent(String(id))}/discard`,
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

  async getUsage(
    params?: { from?: string; to?: string; workspace?: string },
    options?: RequestOptions,
  ): Promise<JsonObject> {
    return this.request<JsonObject>("GET", "/usage", {
      query: params as Record<string, unknown>,
      requestOptions: options,
    });
  }

  async getQuotas(
    params?: { workspace?: string },
    options?: RequestOptions,
  ): Promise<JsonObject> {
    return this.request<JsonObject>("GET", "/quotas", {
      query: params as Record<string, unknown>,
      requestOptions: options,
    });
  }

  async setQuotas(
    input: Record<string, unknown>,
    params?: { workspace?: string },
    options?: RequestOptions,
  ): Promise<JsonObject> {
    return this.request<JsonObject>("PUT", "/quotas", {
      query: params as Record<string, unknown>,
      body: input,
      requestOptions: options,
    });
  }

  async deleteWorkspace(
    id: string,
    params?: DeleteWorkspaceParams,
    options?: RequestOptions,
  ): Promise<JsonObject> {
    const { confirm_header, ...queryParams } = params ?? {};
    const confirmHdr = confirm_header ?? params?.confirm;
    const reqOptions: RequestOptions | undefined = confirmHdr
      ? {
          ...options,
          headers: {
            "X-Confirm-Workspace": confirmHdr,
            ...(options?.headers ?? {}),
          },
        }
      : options;
    return this.request<JsonObject>(
      "DELETE",
      `/workspaces/${encodeURIComponent(String(id))}`,
      {
        query: queryParams as Record<string, unknown>,
        requestOptions: reqOptions,
      },
    );
  }

  async postDeleteWorkspace(
    id: string,
    input?: PostDeleteWorkspaceInput,
    options?: RequestOptions,
  ): Promise<JsonObject> {
    const { confirm_header, ...bodyFields } = input ?? {};
    const confirmHdr = confirm_header ?? input?.confirm;
    const reqOptions: RequestOptions | undefined = confirmHdr
      ? {
          ...options,
          headers: {
            "X-Confirm-Workspace": confirmHdr,
            ...(options?.headers ?? {}),
          },
        }
      : options;
    return this.request<JsonObject>(
      "POST",
      `/workspaces/${encodeURIComponent(String(id))}/delete`,
      {
        body: bodyFields,
        requestOptions: reqOptions,
      },
    );
  }

  async getWorkspaceDeletion(
    id: string,
    params?: { dry_run?: boolean },
    options?: RequestOptions,
  ): Promise<JsonObject> {
    return this.request<JsonObject>(
      "GET",
      `/workspaces/${encodeURIComponent(String(id))}/delete`,
      {
        query: params as Record<string, unknown>,
        requestOptions: options,
      },
    );
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
   * last seen entry `id` as the next `cursor`.
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
      `/templates/${encodeURIComponent(String(id))}`,
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
      `/templates/${encodeURIComponent(String(id))}`,
      { body: input, requestOptions: options },
    );
  }

  async deleteTemplate(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "DELETE",
      `/templates/${encodeURIComponent(String(id))}`,
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
      `/templates/${encodeURIComponent(String(id))}/instantiate`,
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
      `/templates/${encodeURIComponent(String(id))}/instantiate/bulk`,
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
      `/tokens/${encodeURIComponent(String(id))}`,
      { requestOptions: options },
    );
  }

  async revokeToken(id: Int64, options?: RequestOptions): Promise<void> {
    return this.request<void>(
      "POST",
      `/tokens/${encodeURIComponent(String(id))}/revoke`,
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
      `/tokens/${encodeURIComponent(String(id))}/rotate`,
      { body: input ?? {}, requestOptions: options },
    );
  }

  async abandonTokenRotation(
    id: Int64,
    options?: RequestOptions,
  ): Promise<APIToken | void> {
    return this.request<APIToken | void>(
      "POST",
      `/tokens/${encodeURIComponent(String(id))}/abandon`,
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
      `/graphql/operations/${encodeURIComponent(id)}`,
      { requestOptions: options },
    );
  }

  async deleteGraphQLOperation(
    id: string,
    options?: RequestOptions,
  ): Promise<void> {
    return this.request<void>(
      "DELETE",
      `/graphql/operations/${encodeURIComponent(id)}`,
      { requestOptions: options },
    );
  }
}
