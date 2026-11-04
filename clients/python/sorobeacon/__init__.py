# Code generated from internal/api/openapi.json (SoroBeacon API v1.0.0). DO NOT EDIT.
# Regenerate with: go generate ./clients/python/...
# mypy: disable-error-code="no-any-return"
"""Generated Python client for the SoroBeacon API HTTP API (v1.0.0)."""

from __future__ import annotations

import json
import re
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Iterator, Mapping, Sequence
from decimal import Decimal, InvalidOperation
from typing import Any, Literal, NotRequired, Required, TypeAlias, TypedDict

# Wide integer type aliases (resource IDs, ledger sequence numbers, counters,
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


# Stable machine-readable error codes returned in the API Error envelope.
ErrorCode: TypeAlias = Literal[
    "NOT_FOUND",
    "ALREADY_EXISTS",
    "INVALID_ARGUMENT",
    "CONFLICT",
    "UNAUTHENTICATED",
    "PERMISSION_DENIED",
    "RATE_LIMITED",
    "QUOTA_EXCEEDED",
    "PAYLOAD_TOO_LARGE",
    "METHOD_NOT_ALLOWED",
    "NOT_IMPLEMENTED",
    "SERVICE_UNAVAILABLE",
    "INTERNAL_ERROR",
]

# Built-in rule evaluation types.
RuleTypeName: TypeAlias = Literal[
    "event_emitted",
    "value_threshold",
    "token_event",
    "self_transfer",
    "time_window",
    "absence_of_event",
]

# Supported notification delivery channel types.
ChannelType: TypeAlias = Literal[
    "discord",
    "slack",
    "telegram",
    "email",
    "webhook",
    "ntfy",
]

class APIToken(TypedDict, total=False):
    """Redacted on read: the plaintext token secret is returned only once in CreatedAPIToken when minting or rotating a token.
    """

    created_at: Required[ISODateTimeString]
    expires_at: NotRequired[ISODateTimeString | None]
    id: Required[Int64Value]
    last_used_at: NotRequired[ISODateTimeString | None]
    name: Required[str]
    prefix: Required[str]
    revoked_at: NotRequired[ISODateTimeString | None]
    rotated_at: NotRequired[ISODateTimeString | None]
    rotated_from_id: NotRequired[Int64Value]
    scopes: Required[list[str]]
    successor_id: NotRequired[Int64Value]


class Alert(TypedDict, total=False):
    created_at: Required[ISODateTimeString]
    event_id: Required[str]
    event_name: Required[str]
    # True when another SoroBeacon instance forwarded this alert. A federated alert belongs to no local monitor and no local rule, so its monitor_id and rule_id are zero.
    federated: NotRequired[bool]
    # The instance that forwarded the alert. Empty for a local one.
    federation_origin: NotRequired[str]
    id: Required[Int64Value]
    monitor_id: Required[Int64Value]
    monitor_name: NotRequired[str]
    # Stellar chain the matching event came from. Taken from the monitor, never from the caller: it is what a reorg on that chain may retract and no other chain's.
    network: NotRequired[str]
    payload: Required[AlertPayload]
    rule_id: Required[Int64Value]
    # The monitor id on the sending instance. Never a local key.
    source_monitor_id: NotRequired[Int64Value]
    # The rule id on the sending instance. Never a local key.
    source_rule_id: NotRequired[Int64Value]
    # True when a maintenance window silenced delivery
    suppressed: NotRequired[bool]
    suppression_reason: NotRequired[str]


class AlertPage(TypedDict, total=False):
    alerts: Required[list[Alert]]
    # Absent on the last page
    next_cursor: NotRequired[str]


class BulkMonitorInput(TypedDict, total=False):
    enabled: Required[bool]
    ids: Required[list[Int64]]


class BulkMonitorResult(TypedDict, total=False):
    enabled: Required[bool]
    unknown_ids: Required[list[Int64Value]]
    # How many existing monitors changed state
    updated: Required[int]


class Channel(TypedDict, total=False):
    """Redacted on read: channel configuration contains secrets (webhook URLs, bot tokens, SMTP credentials) and is accepted only on ChannelInput.
    """

    # Deliveries that have failed since the last success. One success resets it to 0.
    consecutive_failures: NotRequired[Int64Value]
    # Permanent failures (401/403/404) since the last success. This is what auto-disable counts; a transient 5xx or timeout neither increments nor clears it.
    consecutive_permanent_failures: NotRequired[Int64Value]
    # Set when health tracking auto-disabled the channel, absent when it is on or an operator disabled it. Cleared only by explicitly re-enabling the channel.
    disabled_at: NotRequired[ISODateTimeString]
    enabled: Required[bool]
    id: Required[Int64Value]
    # Most recent failure message, empty once a delivery succeeds. Never contains channel config.
    last_error: NotRequired[str]
    # When last_error was recorded. Absent if there has been no failure since the last success.
    last_error_at: NotRequired[ISODateTimeString]
    # Last delivery that succeeded. Absent until the first one.
    last_success_at: NotRequired[ISODateTimeString]
    name: Required[str]
    # HTTP delivery timeout in seconds (default: 15, range 1-60)
    timeout: NotRequired[int]
    type: Required[ChannelType]


class ChannelDeliveryStats(TypedDict, total=False):
    """One channel's delivery outcomes over the requested window. Attempts are counted, not unique alerts: a retry is another attempt.
    """

    channel_id: Required[Int64Value]
    failed: Required[Int64Value]
    # Null when the window holds no failed attempt
    last_failure_at: NotRequired[ISODateTimeString | None]
    # Null when the window holds no successful attempt
    last_success_at: NotRequired[ISODateTimeString | None]
    succeeded: Required[Int64Value]
    # succeeded / total as a ratio in [0, 1]. Omitted when there were no attempts in the window — an undefined rate is not 0 or 1.
    success_rate: NotRequired[Decimal]
    # Every delivery attempt inside the window, successful or not
    total: Required[Int64Value]
    # The window this response covers, as requested ("24h" when the parameter was omitted)
    window: Required[str]


class ChannelInput(TypedDict, total=False):
    # Type-specific; the channel reference documents each
    config: Required[JsonObject]
    # Empty delivers immediately; window batches into one summary per digest_window_seconds
    digest_mode: NotRequired[Literal["", "window"]]
    # Accumulation window when digest_mode is window; must be > 0 then
    digest_window_seconds: NotRequired[Int64]
    enabled: NotRequired[bool]
    name: Required[str]
    # HTTP delivery timeout in seconds (default: 15, range 1-60)
    timeout: NotRequired[int]
    type: Required[ChannelType]


class ChannelPage(TypedDict, total=False):
    channels: Required[list[Channel]]
    # Absent on the last page
    next_cursor: NotRequired[str]


class CreatedAPIToken(TypedDict, total=False):
    created_at: Required[ISODateTimeString]
    expires_at: NotRequired[ISODateTimeString | None]
    id: Required[Int64Value]
    last_used_at: NotRequired[ISODateTimeString | None]
    name: Required[str]
    prefix: Required[str]
    revoked_at: NotRequired[ISODateTimeString | None]
    rotated_at: NotRequired[ISODateTimeString | None]
    rotated_from_id: NotRequired[Int64Value]
    scopes: Required[list[str]]
    successor_id: NotRequired[Int64Value]
    # Plaintext token secret, returned only once on creation or rotation
    token: Required[str]


class DeliveryAttempt(TypedDict, total=False):
    alert_id: Required[Int64Value]
    attempted_at: Required[ISODateTimeString]
    channel_id: Required[Int64Value]
    response_snippet: NotRequired[str]
    status: Required[Literal["success", "failed"]]


class DryRunResult(TypedDict, total=False):
    # Number of historical events that matched the rule
    count: Required[int]
    # Sample of matching events
    matches: Required[list[JsonObject]]
    # Corpus bias and window documentation
    note: Required[str]
    # Total number of historical events evaluated
    total_evaluated: Required[int]


class ErrorDetail(TypedDict, total=False):
    # Dotted JSON path of the offending field, e.g. rules[0].params.min_amount
    field: Required[str]
    # Human-readable failure for that field
    reason: Required[str]


class Error(TypedDict, total=False):
    # Stable machine-readable error code
    code: NotRequired[ErrorCode]
    # Field-level validation problems. Omitted when there are none so existing clients keep working. Each entry names a JSON path and why it failed; all problems in one request are listed together.
    details: NotRequired[list[ErrorDetail]]
    # Human-readable message
    error: Required[str]
    # Correlation ID, also on the X-Request-ID response header
    request_id: NotRequired[str]


class EscalationInput(TypedDict, total=False):
    # Ordered escalation steps. The first fires immediately; each later step waits its delay after the previous one fired.
    steps: Required[list[JsonObject]]


class EscalationStep(TypedDict, total=False):
    channel_ids: Required[list[Int64Value]]
    # Go duration before this step fires, e.g. 5m
    delay: Required[str]
    # 0-based place in the policy
    position: Required[int]


class EscalationPolicy(TypedDict, total=False):
    created_at: Required[ISODateTimeString]
    id: Required[Int64Value]
    monitor_id: Required[Int64Value]
    steps: Required[list[EscalationStep]]
    updated_at: Required[ISODateTimeString]


class ImportInput(TypedDict, total=False):
    channel_ids: NotRequired[list[Int64]]
    contract_ids: Required[list[str]]
    enabled: NotRequired[bool]
    # Monitor name prefix
    name: Required[str]
    # Chain every monitor in this batch watches. Omitted means the primary. The already-monitored check is per network: the same id on another chain is a different contract, and watching it there is normal.
    network: NotRequired[str]


class ImportRowResult(TypedDict, total=False):
    contract_id: NotRequired[str]
    error: NotRequired[str]
    monitor_id: NotRequired[Int64Value]
    row: NotRequired[int]
    status: NotRequired[Literal["created", "skipped", "error"]]


class Inhibition(TypedDict, total=False):
    created_at: Required[ISODateTimeString]
    # Source counts as firing with an alert inside this window
    firing_window_seconds: Required[int]
    source_rule_id: Required[Int64Value]
    target_rule_id: Required[Int64Value]


class InhibitionInput(TypedDict, total=False):
    # Optional; defaults to 300 when absent or non-positive
    firing_window_seconds: NotRequired[int]
    source_rule_id: Required[Int64]
    target_rule_id: Required[Int64]


class InstantiateInput(TypedDict, total=False):
    contract_ids: Required[list[str]]
    name: Required[str]
    # Chain the instantiated monitor watches. A template is chain-agnostic text, so this defaults to the instance's primary network.
    network: NotRequired[str]
    parameters: NotRequired[dict[str, str]]


class MaintenanceWindow(TypedDict, total=False):
    contract_id: NotRequired[str]
    created_at: Required[ISODateTimeString]
    end_at: Required[ISODateTimeString]
    id: Required[Int64Value]
    monitor_id: NotRequired[Int64Value]
    reason: Required[str]
    scope: Required[Literal["global", "monitor", "contract"]]
    start_at: Required[ISODateTimeString]


class MaintenanceWindowInput(TypedDict, total=False):
    # Required when scope is contract
    contract_id: NotRequired[str]
    # Must be after start_at; open-ended windows are rejected
    end_at: Required[ISODateTimeString]
    # Required when scope is monitor
    monitor_id: NotRequired[Int64]
    reason: Required[str]
    scope: Required[Literal["global", "monitor", "contract"]]
    start_at: Required[ISODateTimeString]


class MaintenanceWindowPage(TypedDict, total=False):
    maintenance_windows: Required[list[MaintenanceWindow]]


class Monitor(TypedDict, total=False):
    channel_ids: NotRequired[list[Int64Value]]
    # Soroban contract strkeys (C...)
    contract_ids: Required[list[str]]
    enabled: Required[bool]
    id: Required[Int64Value]
    name: Required[str]
    # Stellar chain this monitor watches. Empty on rows created before multi-network ingestion, which startup labels with the primary network.
    network: NotRequired[str]
    # Free-form labels for grouping and filtering, normalised to trimmed lowercase unique strings before storage. Always an array; empty when the monitor carries no tags.
    tags: NotRequired[list[str]]


class MonitorInput(TypedDict, total=False):
    channel_ids: NotRequired[list[Int64]]
    contract_ids: Required[list[str]]
    enabled: NotRequired[bool]
    name: Required[str]
    # Stellar chain these contract ids live on, chosen at creation. Omitted means the instance's primary network (the first entry in NETWORKS, or NETWORK when unset). A name this instance does not poll is rejected: the monitor would match nothing forever. A monitor's network cannot be changed afterwards — a contract id resolves to a different contract on another chain, and the alerts already stored keep their old label — so moving one means deleting and recreating it.
    network: NotRequired[str]
    # Free-form labels for grouping and filtering. Values are trimmed, lowercased and deduplicated before storage, so "Treasury" and "treasury" are one tag. Omitted leaves the set unchanged on update and empty on create.
    tags: NotRequired[list[str]]


class MonitorPage(TypedDict, total=False):
    monitors: Required[list[Monitor]]
    # Absent on the last page
    next_cursor: NotRequired[str]


class RuleMatchCount(TypedDict, total=False):
    alerts: NotRequired[Int64Value]
    rule_id: NotRequired[Int64Value]
    type: NotRequired[str]


class MonitorStats(TypedDict, total=False):
    alerts: Required[Int64Value]
    alerts_last_24h: Required[Int64Value]
    alerts_last_7d: Required[Int64Value]
    deliveries_failed: Required[Int64Value]
    deliveries_succeeded: Required[Int64Value]
    # Absent when the monitor has never alerted
    last_alert_at: NotRequired[ISODateTimeString]
    monitor_id: Required[Int64Value]
    # Every rule of the monitor, matched or not
    rules: Required[list[RuleMatchCount]]


class MonitorTemplate(TypedDict, total=False):
    channel_ids: NotRequired[list[Int64Value]]
    created_at: Required[ISODateTimeString]
    description: NotRequired[str]
    id: Required[Int64Value]
    name: Required[str]
    parameters: NotRequired[list[JsonObject]]
    rules: NotRequired[list[JsonObject]]


class MonitorTemplateInput(TypedDict, total=False):
    channel_ids: NotRequired[list[Int64]]
    description: NotRequired[str]
    name: Required[str]
    parameters: NotRequired[list[JsonObject]]
    rules: NotRequired[list[JsonObject]]


class PollerStatus(TypedDict, total=False):
    backing_off: Required[bool]
    last_processed_ledger: NotRequired[Int64Value]
    last_successful_poll: NotRequired[ISODateTimeString]
    latest_chain_ledger: NotRequired[Int64Value]
    ledger_lag: NotRequired[Int64Value]
    status: Required[Literal["waiting", "ok"]]


class Rule(TypedDict, total=False):
    enabled: Required[bool]
    id: Required[Int64Value]
    monitor_id: Required[Int64Value]
    params: Required[JsonObject]
    type: Required[RuleTypeName]


class RuleInput(TypedDict, total=False):
    enabled: NotRequired[bool]
    # Type-specific; validated against the rule type
    params: Required[JsonObject]
    type: Required[RuleTypeName]


class RuleParameter(TypedDict, total=False):
    default: Required[str]
    description: Required[str]
    name: Required[str]
    options: NotRequired[list[str]]
    required: Required[bool]
    type: Required[Literal["string", "number", "object", "select"]]


class RuleType(TypedDict, total=False):
    parameters: Required[list[RuleParameter]]
    type: Required[str]


class RuleTypesResponse(TypedDict, total=False):
    rule_types: Required[list[RuleType]]


class TokenInput(TypedDict, total=False):
    # Go duration string (e.g. 168h); omit for a token that never expires
    expires_in: NotRequired[str]
    name: Required[str]
    scopes: Required[list[str]]


class Version(TypedDict, total=False):
    build_date: Required[str]
    commit: Required[str]
    version: Required[str]


class MonitorUpdateInput(TypedDict, total=False):
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


class DeleteWorkspaceParams(TypedDict, total=False):
    """Query and header parameters for DELETE /workspaces/{id}."""

    dry_run: bool
    confirm: str
    batch_size: int
    max_alert_batches: int
    confirm_header: str


class PostDeleteWorkspaceInput(TypedDict, total=False):
    """Request payload and header parameters for POST /workspaces/{id}/delete."""

    dry_run: bool
    confirm: str
    batch_size: int
    max_alert_batches: int
    confirm_header: str


MAX_SAFE_INT = 9007199254740991
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


# Complete table of OpenAPI operations covered by SoroBeaconClient.
OPENAPI_OPERATIONS: tuple[dict[str, str], ...] = (
    {"method": "GET", "path": "/monitors", "client_method": "list_monitors", "summary": "List monitors"},
    {"method": "POST", "path": "/monitors", "client_method": "create_monitor", "summary": "Create a monitor"},
    {"method": "POST", "path": "/monitors/bulk", "client_method": "bulk_update_monitors", "summary": "Bulk enable or disable monitors"},
    {"method": "POST", "path": "/monitors/import", "client_method": "import_monitors", "summary": "Bulk import contracts from a file"},
    {"method": "GET", "path": "/monitors/trash", "client_method": "list_trash_monitors", "summary": "List deleted monitors (trash)"},
    {"method": "GET", "path": "/monitors/{id}", "client_method": "get_monitor", "summary": "Fetch one monitor"},
    {"method": "PATCH", "path": "/monitors/{id}", "client_method": "update_monitor", "summary": "Update a monitor"},
    {"method": "DELETE", "path": "/monitors/{id}", "client_method": "delete_monitor", "summary": "Delete a monitor"},
    {"method": "POST", "path": "/monitors/{id}/soft-delete", "client_method": "soft_delete_monitor", "summary": "Soft-delete a monitor"},
    {"method": "POST", "path": "/monitors/{id}/restore", "client_method": "restore_monitor", "summary": "Restore a soft-deleted monitor"},
    {"method": "POST", "path": "/monitors/{id}/duplicate", "client_method": "duplicate_monitor", "summary": "Duplicate a monitor"},
    {"method": "GET", "path": "/monitors/{id}/stats", "client_method": "get_monitor_stats", "summary": "Per-monitor statistics"},
    {"method": "GET", "path": "/monitors/{id}/escalation", "client_method": "get_monitor_escalation", "summary": "Get a monitor's escalation policy"},
    {"method": "PUT", "path": "/monitors/{id}/escalation", "client_method": "set_monitor_escalation", "summary": "Create or replace a monitor's escalation policy"},
    {"method": "DELETE", "path": "/monitors/{id}/escalation", "client_method": "delete_monitor_escalation", "summary": "Delete a monitor's escalation policy"},
    {"method": "GET", "path": "/monitors/{id}/rules", "client_method": "list_rules", "summary": "List a monitor's rules"},
    {"method": "POST", "path": "/monitors/{id}/rules", "client_method": "create_rule", "summary": "Add a rule to a monitor"},
    {"method": "POST", "path": "/monitors/{id}/rules/bulk", "client_method": "bulk_create_rules", "summary": "Create several rules in one request"},
    {"method": "POST", "path": "/monitors/{id}/rules/dry-run", "client_method": "dry_run_rule", "summary": "Dry-run a rule against historical events"},
    {"method": "PATCH", "path": "/monitors/{id}/rules/{ruleID}", "client_method": "update_rule", "summary": "Update a rule"},
    {"method": "DELETE", "path": "/monitors/{id}/rules/{ruleID}", "client_method": "delete_rule", "summary": "Delete a rule"},
    {"method": "GET", "path": "/channels", "client_method": "list_channels", "summary": "List channels"},
    {"method": "POST", "path": "/channels", "client_method": "create_channel", "summary": "Create a notification channel"},
    {"method": "POST", "path": "/channels/bulk", "client_method": "bulk_update_channels", "summary": "Bulk enable or disable channels"},
    {"method": "GET", "path": "/channels/{id}", "client_method": "get_channel", "summary": "Fetch one channel"},
    {"method": "PATCH", "path": "/channels/{id}", "client_method": "update_channel", "summary": "Update a channel"},
    {"method": "DELETE", "path": "/channels/{id}", "client_method": "delete_channel", "summary": "Delete a channel"},
    {"method": "GET", "path": "/channels/{id}/stats", "client_method": "get_channel_stats", "summary": "Per-channel delivery statistics"},
    {"method": "POST", "path": "/channels/{id}/duplicate", "client_method": "duplicate_channel", "summary": "Duplicate a channel"},
    {"method": "POST", "path": "/channels/{id}/test", "client_method": "test_channel", "summary": "Send a test notification through a channel"},
    {"method": "GET", "path": "/channels/{id}/keys", "client_method": "list_channel_keys", "summary": "List signing keys for a channel"},
    {"method": "POST", "path": "/channels/{id}/keys/rotate", "client_method": "rotate_channel_key", "summary": "Rotate channel signing key with overlap"},
    {"method": "POST", "path": "/channels/{id}/keys/{keyID}/retire", "client_method": "retire_channel_key", "summary": "Retire a channel signing key"},
    {"method": "POST", "path": "/channels/{id}/keys/{keyID}/revoke", "client_method": "revoke_channel_key", "summary": "Revoke a channel signing key immediately"},
    {"method": "GET", "path": "/maintenance-windows", "client_method": "list_maintenance_windows", "summary": "List maintenance windows"},
    {"method": "POST", "path": "/maintenance-windows", "client_method": "create_maintenance_window", "summary": "Create a maintenance window"},
    {"method": "GET", "path": "/maintenance-windows/{id}", "client_method": "get_maintenance_window", "summary": "Fetch one maintenance window"},
    {"method": "PATCH", "path": "/maintenance-windows/{id}", "client_method": "update_maintenance_window", "summary": "Update a maintenance window"},
    {"method": "DELETE", "path": "/maintenance-windows/{id}", "client_method": "delete_maintenance_window", "summary": "Delete a maintenance window"},
    {"method": "GET", "path": "/inhibitions", "client_method": "list_inhibitions", "summary": "List inhibition rules"},
    {"method": "POST", "path": "/inhibitions", "client_method": "create_inhibition", "summary": "Create an inhibition rule"},
    {"method": "DELETE", "path": "/inhibitions/{sourceID}/{targetID}", "client_method": "delete_inhibition", "summary": "Delete an inhibition rule"},
    {"method": "GET", "path": "/alerts", "client_method": "list_alerts", "summary": "List alerts"},
    {"method": "GET", "path": "/alerts/stream", "client_method": "stream_alerts", "summary": "Stream alerts as they are created (Server-Sent Events)"},
    {"method": "GET", "path": "/alerts.csv", "client_method": "export_alerts_csv", "summary": "Export alerts as CSV"},
    {"method": "GET", "path": "/alerts/export", "client_method": "export_alerts_ndjson", "summary": "Export alerts as NDJSON"},
    {"method": "POST", "path": "/alerts/{id}/acknowledge", "client_method": "acknowledge_alert", "summary": "Acknowledge an alert"},
    {"method": "GET", "path": "/alerts/{id}/deliveries", "client_method": "list_alert_deliveries", "summary": "List one alert's delivery attempts"},
    {"method": "POST", "path": "/alerts/{id}/deliveries/{channelID}/retry", "client_method": "retry_alert_delivery", "summary": "Retry a failed delivery"},
    {"method": "GET", "path": "/dead-letters", "client_method": "list_dead_letters", "summary": "List dead-letter queue records"},
    {"method": "GET", "path": "/dead-letters/groups", "client_method": "list_dead_letter_groups", "summary": "Roll up dead letters into groups by channel and cause"},
    {"method": "POST", "path": "/dead-letters/bulk/redrive", "client_method": "bulk_redrive_dead_letters", "summary": "Re-drive a batch of dead letters matching filter or IDs"},
    {"method": "POST", "path": "/dead-letters/bulk/discard", "client_method": "bulk_discard_dead_letters", "summary": "Discard a batch of dead letters matching filter or IDs"},
    {"method": "POST", "path": "/dead-letters/{id}/redrive", "client_method": "redrive_dead_letter", "summary": "Re-drive a dead-letter delivery"},
    {"method": "POST", "path": "/dead-letters/{id}/discard", "client_method": "discard_dead_letter", "summary": "Discard one dead letter from the backlog"},
    {"method": "POST", "path": "/ingest", "client_method": "ingest_federated_alert", "summary": "Receive a federated alert from another SoroBeacon instance"},
    {"method": "POST", "path": "/triggers", "client_method": "trigger_evaluation", "summary": "Trigger alert evaluation from an external event"},
    {"method": "GET", "path": "/health", "client_method": "get_health", "summary": "Combined health report"},
    {"method": "GET", "path": "/livez", "client_method": "get_livez", "summary": "Liveness probe"},
    {"method": "GET", "path": "/readyz", "client_method": "get_readyz", "summary": "Readiness probe"},
    {"method": "GET", "path": "/poller", "client_method": "get_poller_status", "summary": "Poller position and lag"},
    {"method": "GET", "path": "/version", "client_method": "get_version", "summary": "Build information"},
    {"method": "GET", "path": "/rule-types", "client_method": "list_rule_types", "summary": "List rule types and parameter schemas"},
    {"method": "GET", "path": "/stats", "client_method": "get_stats", "summary": "Service statistics"},
    {"method": "GET", "path": "/stats/alerts-daily", "client_method": "get_daily_alert_counts", "summary": "Daily alert counts for the last 30 UTC days"},
    {"method": "GET", "path": "/usage", "client_method": "get_usage", "summary": "Per-workspace usage metering summary"},
    {"method": "GET", "path": "/quotas", "client_method": "get_quotas", "summary": "Per-workspace resource quotas and query cost budget status"},
    {"method": "PUT", "path": "/quotas", "client_method": "set_quotas", "summary": "Configure per-workspace resource quotas and query cost budget"},
    {"method": "DELETE", "path": "/workspaces/{id}", "client_method": "delete_workspace", "summary": "Delete a workspace and everything belonging to it (or preview via dry-run)"},
    {"method": "POST", "path": "/workspaces/{id}/delete", "client_method": "post_delete_workspace", "summary": "Delete a workspace and everything belonging to it (or preview via dry-run)"},
    {"method": "GET", "path": "/workspaces/{id}/delete", "client_method": "get_workspace_deletion", "summary": "Inspect workspace deletion progress or completion tombstone"},
    {"method": "GET", "path": "/retention", "client_method": "get_retention", "summary": "Get effective workspace alert retention policy"},
    {"method": "PUT", "path": "/retention", "client_method": "set_retention", "summary": "Set workspace alert retention policy override"},
    {"method": "DELETE", "path": "/retention", "client_method": "reset_retention", "summary": "Reset workspace alert retention policy to instance default"},
    {"method": "GET", "path": "/workspace/export", "client_method": "export_current_workspace", "summary": "Stream a single-workspace data export for the caller's workspace"},
    {"method": "GET", "path": "/workspaces/{id}/export", "client_method": "export_workspace", "summary": "Stream a single-workspace data export by workspace ID"},
    {"method": "GET", "path": "/audit", "client_method": "list_audit_logs", "summary": "Audit log of configuration changes"},
    {"method": "GET", "path": "/templates", "client_method": "list_templates", "summary": "List monitor templates"},
    {"method": "POST", "path": "/templates", "client_method": "create_template", "summary": "Create a monitor template"},
    {"method": "GET", "path": "/templates/{id}", "client_method": "get_template", "summary": "Get a monitor template"},
    {"method": "PATCH", "path": "/templates/{id}", "client_method": "update_template", "summary": "Update a monitor template"},
    {"method": "DELETE", "path": "/templates/{id}", "client_method": "delete_template", "summary": "Delete a monitor template"},
    {"method": "POST", "path": "/templates/{id}/instantiate", "client_method": "instantiate_template", "summary": "Create a monitor from a template"},
    {"method": "POST", "path": "/templates/{id}/instantiate/bulk", "client_method": "bulk_instantiate_template", "summary": "Bulk create monitors from a template"},
    {"method": "GET", "path": "/tokens", "client_method": "list_tokens", "summary": "List API tokens"},
    {"method": "POST", "path": "/tokens", "client_method": "create_token", "summary": "Mint a scoped API token"},
    {"method": "GET", "path": "/tokens/{id}", "client_method": "get_token", "summary": "Get an API token by ID"},
    {"method": "POST", "path": "/tokens/{id}/revoke", "client_method": "revoke_token", "summary": "Revoke an API token"},
    {"method": "POST", "path": "/tokens/{id}/rotate", "client_method": "rotate_token", "summary": "Rotate an API token with overlap"},
    {"method": "POST", "path": "/tokens/{id}/abandon", "client_method": "abandon_token_rotation", "summary": "Abandon an in-progress token rotation"},
    {"method": "GET", "path": "/graphql/operations", "client_method": "list_graphql_operations", "summary": "List registered persisted GraphQL operations"},
    {"method": "POST", "path": "/graphql/operations", "client_method": "register_graphql_operation", "summary": "Register a persisted GraphQL operation"},
    {"method": "GET", "path": "/graphql/operations/{id}", "client_method": "get_graphql_operation", "summary": "Get a registered persisted GraphQL operation"},
    {"method": "DELETE", "path": "/graphql/operations/{id}", "client_method": "delete_graphql_operation", "summary": "Delete a registered persisted GraphQL operation"},
)


class SoroBeaconClient:
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

    def get_usage(
        self,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request("GET", "/usage", query=params, headers=headers)

    def get_quotas(
        self,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request("GET", "/quotas", query=params, headers=headers)

    def set_quotas(
        self,
        input_data: Mapping[str, Any],
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "PUT",
            "/quotas",
            query=params,
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def delete_workspace(
        self,
        workspace_id: str,
        params: DeleteWorkspaceParams | Mapping[str, Any] | None = None,
        *,
        dry_run: bool | None = None,
        confirm: str | None = None,
        batch_size: int | None = None,
        max_alert_batches: int | None = None,
        confirm_header: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        query: dict[str, Any] = dict(params) if params else {}
        hdr_confirm = query.pop("confirm_header", None)
        if dry_run is not None:
            query["dry_run"] = dry_run
        if confirm is not None:
            query["confirm"] = confirm
        if batch_size is not None:
            query["batch_size"] = batch_size
        if max_alert_batches is not None:
            query["max_alert_batches"] = max_alert_batches
        req_headers: dict[str, str] = dict(headers) if headers else {}
        effective_confirm_hdr = confirm_header or hdr_confirm or query.get("confirm")
        if effective_confirm_hdr and "X-Confirm-Workspace" not in req_headers:
            req_headers["X-Confirm-Workspace"] = str(effective_confirm_hdr)
        return self._request(
            "DELETE",
            f"/workspaces/{self._quote(workspace_id)}",
            query=query or None,
            headers=req_headers or None,
        )

    def post_delete_workspace(
        self,
        workspace_id: str,
        input_data: PostDeleteWorkspaceInput | Mapping[str, Any] | None = None,
        *,
        dry_run: bool | None = None,
        confirm: str | None = None,
        batch_size: int | None = None,
        max_alert_batches: int | None = None,
        confirm_header: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        body: dict[str, Any] = dict(input_data) if input_data else {}
        hdr_confirm = body.pop("confirm_header", None)
        if dry_run is not None:
            body["dry_run"] = dry_run
        if confirm is not None:
            body["confirm"] = confirm
        if batch_size is not None:
            body["batch_size"] = batch_size
        if max_alert_batches is not None:
            body["max_alert_batches"] = max_alert_batches
        req_headers: dict[str, str] = dict(headers) if headers else {}
        effective_confirm_hdr = confirm_header or hdr_confirm or body.get("confirm")
        if effective_confirm_hdr and "X-Confirm-Workspace" not in req_headers:
            req_headers["X-Confirm-Workspace"] = str(effective_confirm_hdr)
        return self._request(
            "POST",
            f"/workspaces/{self._quote(workspace_id)}/delete",
            body=body,
            has_body=True,
            headers=req_headers or None,
        )

    def get_workspace_deletion(
        self,
        workspace_id: str,
        params: Mapping[str, Any] | None = None,
        *,
        dry_run: bool | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        query: dict[str, Any] = dict(params) if params else {}
        if dry_run is not None:
            query["dry_run"] = dry_run
        return self._request(
            "GET",
            f"/workspaces/{self._quote(workspace_id)}/delete",
            query=query or None,
            headers=headers,
        )

    def get_retention(
        self,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request("GET", "/retention", query=params, headers=headers)

    def set_retention(
        self,
        input_data: Mapping[str, Any],
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "PUT",
            "/retention",
            query=params,
            body=input_data,
            has_body=True,
            headers=headers,
        )

    def reset_retention(
        self,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request("DELETE", "/retention", query=params, headers=headers)

    def export_current_workspace(
        self,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request("GET", "/workspace/export", query=params, headers=headers)

    def export_workspace(
        self,
        workspace_id: str,
        params: Mapping[str, Any] | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JsonObject:
        return self._request(
            "GET",
            f"/workspaces/{urllib.parse.quote(str(workspace_id), safe='')}/export",
            query=params,
            headers=headers,
        )

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
