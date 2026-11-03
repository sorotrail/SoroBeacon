"""Runtime transport, cursor pagination iterators, wide-integer helpers, and typed errors."""

from __future__ import annotations

from collections.abc import Callable, Iterator, Mapping, Sequence
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation
import http.client
import json
from typing import Final, Generic, TypeVar
import urllib.error
import urllib.parse
import urllib.request

from ._generated import (
    APIToken,
    Alert,
    AlertPage,
    BulkMonitorInput,
    BulkMonitorResult,
    Channel,
    ChannelDeliveryStats,
    ChannelInput,
    ChannelPage,
    CreatedAPIToken,
    DeliveryAttempt,
    DryRunResult,
    ErrorCode,
    ErrorDetail,
    EscalationInput,
    EscalationPolicy,
    ImportInput,
    ImportRowResult,
    Inhibition,
    InhibitionInput,
    InstantiateInput,
    Int64,
    JSONObject,
    JSONValue,
    MaintenanceWindow,
    MaintenanceWindowInput,
    MaintenanceWindowPage,
    Monitor,
    MonitorInput,
    MonitorPage,
    MonitorStats,
    MonitorTemplate,
    MonitorTemplateInput,
    PollerStatus,
    Rule,
    RuleInput,
    RuleTypesResponse,
    TokenInput,
    Version,
    WideInt,
    _as_int,
    _as_json_obj,
    _as_json_obj_list,
    _as_json_val,
    _as_list,
    _as_opt_int,
    _as_opt_json_obj,
    _as_opt_str,
    _as_str,
    _is_float_instance,
)

ItemT = TypeVar("ItemT")

I128_MIN: Final[int] = -(1 << 127)
I128_MAX: Final[int] = (1 << 127) - 1
U128_MAX: Final[int] = (1 << 128) - 1
I256_MIN: Final[int] = -(1 << 255)
I256_MAX: Final[int] = (1 << 255) - 1
U256_MAX: Final[int] = (1 << 256) - 1


def parse_wide_int(value: WideInt | int) -> int:
    """Parse a Soroban i128/u128/i256/u256 or 64-bit value into an exact Python int.

    Rejects IEEE-754 float and bool values with TypeError so wide integers
    never suffer silent 53-bit mantissa truncation.
    """
    if isinstance(value, bool) or _is_float_instance(value):
        raise TypeError("float and bool are not valid wide integers; pass str, Decimal, or int")
    if isinstance(value, int):
        parsed = value
    elif isinstance(value, Decimal):
        if not value.is_finite() or value != value.to_integral_value():
            raise ValueError(f"non-integral Decimal cannot be converted to wide int: {value!r}")
        parsed = int(value)
    elif isinstance(value, str):
        stripped = value.strip()
        if stripped == "":
            raise ValueError("empty string is not a valid wide integer")
        try:
            parsed = int(stripped, 10)
        except ValueError as exc:
            raise ValueError(f"invalid wide integer string: {value!r}") from exc
    else:
        raise TypeError(f"unsupported wide integer type: {type(value).__name__}")

    if parsed < I256_MIN or parsed > U256_MAX:
        raise ValueError(f"value {parsed} exceeds 256-bit integer bounds [{I256_MIN}, {U256_MAX}]")
    return parsed


def parse_wide_decimal(value: WideInt | int, decimals: int = 0) -> Decimal:
    """Parse a Soroban i128/u128/i256/u256 value into a high-precision Decimal.

    When decimals > 0, scales the integer value by 10 ** -decimals using exact
    base-10 arithmetic (e.g. decimals=7 for Stellar SAC tokens or decimals=18
    for 18-decimal SEP-41 tokens).
    """
    if isinstance(value, bool) or _is_float_instance(value):
        raise TypeError("float and bool are not valid wide decimals; pass str, Decimal, or int")
    if isinstance(decimals, bool) or not isinstance(decimals, int) or decimals < 0:
        raise ValueError("decimals must be a non-negative integer")
    if isinstance(value, int):
        dec = Decimal(value)
    elif isinstance(value, Decimal):
        if not value.is_finite():
            raise ValueError(f"non-finite Decimal is not a valid wide decimal: {value!r}")
        dec = value
    elif isinstance(value, str):
        stripped = value.strip()
        if stripped == "":
            raise ValueError("empty string is not a valid wide decimal")
        try:
            dec = Decimal(stripped)
        except InvalidOperation as exc:
            raise ValueError(f"invalid wide decimal string: {value!r}") from exc
        if not dec.is_finite():
            raise ValueError(f"non-finite wide decimal string: {value!r}")
    else:
        raise TypeError(f"unsupported wide decimal type: {type(value).__name__}")

    if decimals == 0:
        return dec
    tup = dec.as_tuple()
    if not isinstance(tup.exponent, int):
        raise ValueError(f"non-finite Decimal is not a valid wide decimal: {value!r}")
    return Decimal((tup.sign, tup.digits, tup.exponent - decimals))


def format_wide_int(value: WideInt | int) -> str:
    """Format a wide integer as a canonical base-10 string without float precision loss."""
    return str(parse_wide_int(value))


def parse_json_preserving_wide_ints(raw: str) -> JSONValue:
    """Parse a JSON string while decoding all non-integer numbers as exact Decimal."""
    loaded: object = json.loads(raw, parse_float=Decimal)
    return _as_json_val(loaded)


def _prepare_for_json(val: object) -> object:
    if val is None or isinstance(val, (str, bool)):
        return val
    if _is_float_instance(val):
        raise TypeError("float is not permitted in SoroBeacon JSON payloads; use Decimal, int, or str")
    if isinstance(val, int):
        return val
    if isinstance(val, Decimal):
        if not val.is_finite():
            raise ValueError(f"non-finite Decimal cannot be serialized to JSON: {val!r}")
        if val == val.to_integral_value():
            return int(val)
        return str(val)
    if hasattr(val, "to_dict") and callable(getattr(val, "to_dict")):
        as_dict: object = getattr(val, "to_dict")()
        return _prepare_for_json(as_dict)
    if isinstance(val, Mapping):
        return {str(k): _prepare_for_json(v) for k, v in val.items() if v is not None}
    if isinstance(val, (list, tuple)):
        return [_prepare_for_json(item) for item in val]
    return str(val)


def dumps_json_with_wide_ints(value: object) -> str:
    """Serialize a Python object to JSON without IEEE-754 float coercion."""
    prepared = _prepare_for_json(value)
    return json.dumps(prepared, separators=(",", ":"), allow_nan=False)


class SoroBeaconError(Exception):
    """Base exception raised by the SoroBeacon Python client."""


class SoroBeaconAPIError(SoroBeaconError):
    """Structured HTTP error returned by a SoroBeacon server."""

    status_code: int
    code: str
    message: str
    request_id: str | None
    details: list[ErrorDetail]
    retry_after: int | None

    def __init__(
        self,
        status_code: int,
        message: str,
        *,
        code: str = "",
        request_id: str | None = None,
        details: Sequence[ErrorDetail] | None = None,
        retry_after: int | None = None,
    ) -> None:
        resolved_code = code if code != "" else _default_code_for_status(status_code)
        resolved_req_id = request_id if (request_id is not None and request_id != "") else None
        resolved_details = list(details) if details is not None else []
        super().__init__(self._format(status_code, resolved_code, message, resolved_req_id))
        self.status_code = status_code
        self.code = resolved_code
        self.message = message
        self.request_id = resolved_req_id
        self.details = resolved_details
        self.retry_after = retry_after

    @staticmethod
    def _format(
        status_code: int,
        code: str,
        message: str,
        request_id: str | None,
    ) -> str:
        prefix = f"sorobeacon: status {status_code} ({code})"
        if request_id is not None:
            return f"{prefix} [request_id={request_id}]: {message}"
        return f"{prefix}: {message}"

    def is_not_found(self) -> bool:
        return self.code == ErrorCode.NOT_FOUND.value or self.status_code == 404

    def is_already_exists(self) -> bool:
        return self.code == ErrorCode.ALREADY_EXISTS.value

    def is_invalid_argument(self) -> bool:
        return (
            self.code == ErrorCode.INVALID_ARGUMENT.value
            or self.status_code == 400
            or self.status_code == 422
        )

    def is_conflict(self) -> bool:
        return self.code == ErrorCode.CONFLICT.value or self.status_code == 409

    def is_unauthenticated(self) -> bool:
        return self.code == ErrorCode.UNAUTHENTICATED.value or self.status_code == 401

    def is_permission_denied(self) -> bool:
        return self.code == ErrorCode.PERMISSION_DENIED.value or self.status_code == 403

    def is_rate_limited(self) -> bool:
        return self.code == ErrorCode.RATE_LIMITED.value or self.status_code == 429

    def is_quota_exceeded(self) -> bool:
        return self.code == ErrorCode.QUOTA_EXCEEDED.value

    def is_payload_too_large(self) -> bool:
        return self.code == ErrorCode.PAYLOAD_TOO_LARGE.value or self.status_code == 413

    def is_service_unavailable(self) -> bool:
        return self.code == ErrorCode.SERVICE_UNAVAILABLE.value or self.status_code == 503


class BadRequestError(SoroBeaconAPIError):
    """Raised on HTTP 400/422 validation or invalid argument errors."""


class UnauthenticatedError(SoroBeaconAPIError):
    """Raised on HTTP 401 authentication errors."""


class PermissionDeniedError(SoroBeaconAPIError):
    """Raised on HTTP 403 authorization errors."""


class NotFoundError(SoroBeaconAPIError):
    """Raised on HTTP 404 missing resource errors."""


class AlreadyExistsError(SoroBeaconAPIError):
    """Raised when creating a resource that already exists (ALREADY_EXISTS)."""


class ConflictError(SoroBeaconAPIError):
    """Raised on HTTP 409 state conflict errors."""


class PayloadTooLargeError(SoroBeaconAPIError):
    """Raised on HTTP 413 oversized request body errors."""


class RateLimitedError(SoroBeaconAPIError):
    """Raised on HTTP 429 rate-limit exhaustion (RATE_LIMITED)."""


class QuotaExceededError(SoroBeaconAPIError):
    """Raised on HTTP 429 workspace quota exhaustion (QUOTA_EXCEEDED)."""


class InternalServerError(SoroBeaconAPIError):
    """Raised on HTTP 500 internal server errors."""


class ServiceUnavailableError(SoroBeaconAPIError):
    """Raised on HTTP 503 service unavailable errors."""


def _default_code_for_status(status_code: int) -> str:
    mapping: dict[int, str] = {
        400: ErrorCode.INVALID_ARGUMENT.value,
        401: ErrorCode.UNAUTHENTICATED.value,
        403: ErrorCode.PERMISSION_DENIED.value,
        404: ErrorCode.NOT_FOUND.value,
        405: ErrorCode.METHOD_NOT_ALLOWED.value,
        409: ErrorCode.CONFLICT.value,
        413: ErrorCode.PAYLOAD_TOO_LARGE.value,
        422: ErrorCode.INVALID_ARGUMENT.value,
        429: ErrorCode.RATE_LIMITED.value,
        500: ErrorCode.INTERNAL_ERROR.value,
        501: ErrorCode.NOT_IMPLEMENTED.value,
        503: ErrorCode.SERVICE_UNAVAILABLE.value,
    }
    return mapping.get(status_code, ErrorCode.INTERNAL_ERROR.value)


def _build_api_error(
    status_code: int,
    message: str,
    *,
    code: str = "",
    request_id: str | None = None,
    details: Sequence[ErrorDetail] | None = None,
    retry_after: int | None = None,
) -> SoroBeaconAPIError:
    resolved_code = code if code != "" else _default_code_for_status(status_code)
    if resolved_code == ErrorCode.QUOTA_EXCEEDED.value:
        err_cls: type[SoroBeaconAPIError] = QuotaExceededError
    elif resolved_code == ErrorCode.ALREADY_EXISTS.value:
        err_cls = AlreadyExistsError
    else:
        cls_by_status: dict[int, type[SoroBeaconAPIError]] = {
            400: BadRequestError,
            401: UnauthenticatedError,
            403: PermissionDeniedError,
            404: NotFoundError,
            409: ConflictError,
            413: PayloadTooLargeError,
            422: BadRequestError,
            429: RateLimitedError,
            500: InternalServerError,
            503: ServiceUnavailableError,
        }
        err_cls = cls_by_status.get(status_code, SoroBeaconAPIError)

    return err_cls(
        status_code,
        message,
        code=resolved_code,
        request_id=request_id,
        details=details,
        retry_after=retry_after,
    )


class CursorIterator(Generic[ItemT], Iterator[ItemT]):
    """Lazy iterator that transparently walks cursor-paginated SoroBeacon endpoints."""

    _fetch_page: Callable[[str | None], tuple[list[ItemT], str | None]]
    _buffer: list[ItemT]
    _index: int
    _next_cursor: str | None
    _started: bool
    _exhausted: bool

    def __init__(
        self,
        fetch_page: Callable[[str | None], tuple[list[ItemT], str | None]],
    ) -> None:
        self._fetch_page = fetch_page
        self._buffer = []
        self._index = 0
        self._next_cursor = None
        self._started = False
        self._exhausted = False

    def __iter__(self) -> CursorIterator[ItemT]:
        return self

    def __next__(self) -> ItemT:
        while self._index >= len(self._buffer):
            if self._exhausted:
                raise StopIteration
            cursor_to_send = self._next_cursor if self._started else None
            self._started = True
            items, next_cursor = self._fetch_page(cursor_to_send)
            self._buffer = items
            self._index = 0
            if not items or next_cursor is None or next_cursor == "" or next_cursor == "0":
                self._exhausted = True
            else:
                self._next_cursor = next_cursor
            if not self._buffer:
                raise StopIteration

        item = self._buffer[self._index]
        self._index += 1
        return item


@dataclass(slots=True)
class AuditEntry:
    """Single entry in the append-only configuration audit log (GET /audit)."""

    id: int = 0
    actor: str = ""
    action: str = ""
    target_type: str = ""
    target_id: int = 0
    created_at: str = ""
    target_name: str | None = None
    diff: JSONObject | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> AuditEntry:
        return cls(
            id=_as_int(data.get("id")),
            actor=_as_str(data.get("actor")),
            action=_as_str(data.get("action")),
            target_type=_as_str(data.get("target_type")),
            target_id=_as_int(data.get("target_id")),
            created_at=_as_str(data.get("created_at")),
            target_name=_as_opt_str(data.get("target_name")),
            diff=_as_opt_json_obj(data.get("diff")),
        )


@dataclass(slots=True)
class AuditPage:
    """Response envelope for GET /audit."""

    entries: list[AuditEntry] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> AuditPage:
        return cls(
            entries=[
                AuditEntry.from_dict(item)
                for item in _as_list(data.get("entries"))
                if isinstance(item, Mapping)
            ]
        )


@dataclass(slots=True)
class DailyAlertCount:
    """Daily alert count bucket returned by GET /stats/alerts-daily."""

    day: str = ""
    count: int = 0

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> DailyAlertCount:
        return cls(
            day=_as_str(data.get("day")),
            count=_as_int(data.get("count")),
        )


@dataclass(slots=True)
class DailyAlertCountsResponse:
    """Response envelope for GET /stats/alerts-daily."""

    timezone: str = ""
    days: list[DailyAlertCount] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> DailyAlertCountsResponse:
        return cls(
            timezone=_as_str(data.get("timezone")),
            days=[
                DailyAlertCount.from_dict(item)
                for item in _as_list(data.get("days"))
                if isinstance(item, Mapping)
            ],
        )


@dataclass(slots=True)
class AlertPayload:
    """Structured decoded Soroban contract event payload attached to an Alert."""

    contract_id: str | None = None
    event_name: str | None = None
    ledger: int | None = None
    topics: list[JSONValue] | None = None
    data: JSONValue = None
    value: JSONValue = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> AlertPayload:
        topics_raw = data.get("topics")
        topics: list[JSONValue] | None = None
        if isinstance(topics_raw, list):
            topics = [_as_json_val(item) for item in topics_raw]
        return cls(
            contract_id=_as_opt_str(data.get("contract_id")),
            event_name=_as_opt_str(data.get("event_name")),
            ledger=_as_opt_int(data.get("ledger")),
            topics=topics,
            data=_as_json_val(data.get("data")),
            value=_as_json_val(data.get("value")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.contract_id is not None:
            out["contract_id"] = self.contract_id
        if self.event_name is not None:
            out["event_name"] = self.event_name
        if self.ledger is not None:
            out["ledger"] = self.ledger
        if self.topics is not None:
            out["topics"] = self.topics
        if self.data is not None:
            out["data"] = self.data
        if self.value is not None:
            out["value"] = self.value
        return out


@dataclass(slots=True)
class MonitorUpdateInput:
    """Partial update payload for PATCH /monitors/{id}."""

    name: str | None = None
    contract_ids: list[str] | None = None
    enabled: bool | None = None
    network: str | None = None
    tags: list[str] | None = None
    channel_ids: list[int] | None = None

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.name is not None:
            out["name"] = self.name
        if self.contract_ids is not None:
            out["contract_ids"] = _as_json_val(self.contract_ids)
        if self.enabled is not None:
            out["enabled"] = self.enabled
        if self.network is not None:
            out["network"] = self.network
        if self.tags is not None:
            out["tags"] = _as_json_val(self.tags)
        if self.channel_ids is not None:
            out["channel_ids"] = _as_json_val(self.channel_ids)
        return out


@dataclass(slots=True)
class RuleUpdateInput:
    """Partial update payload for PATCH /monitors/{id}/rules/{ruleID}."""

    type: str | None = None
    params: JSONObject | None = None
    enabled: bool | None = None

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.type is not None:
            out["type"] = self.type
        if self.params is not None:
            out["params"] = _as_json_val(self.params)
        if self.enabled is not None:
            out["enabled"] = self.enabled
        return out


@dataclass(slots=True)
class ChannelUpdateInput:
    """Partial update payload for PATCH /channels/{id}.

    config is accepted on write when rotating or updating channel credentials,
    and is never returned in Channel read responses.
    """

    name: str | None = None
    type: str | None = None
    config: JSONObject | None = None
    enabled: bool | None = None
    digest_mode: str | None = None
    digest_window_seconds: int | None = None
    timeout: int | None = None

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.name is not None:
            out["name"] = self.name
        if self.type is not None:
            out["type"] = self.type
        if self.config is not None:
            out["config"] = _as_json_val(self.config)
        if self.enabled is not None:
            out["enabled"] = self.enabled
        if self.digest_mode is not None:
            out["digest_mode"] = self.digest_mode
        if self.digest_window_seconds is not None:
            out["digest_window_seconds"] = self.digest_window_seconds
        if self.timeout is not None:
            out["timeout"] = self.timeout
        return out


@dataclass(slots=True)
class MaintenanceWindowUpdateInput:
    """Partial update payload for PATCH /maintenance-windows/{id}."""

    reason: str | None = None
    scope: str | None = None
    monitor_id: int | None = None
    contract_id: str | None = None
    start_at: str | None = None
    end_at: str | None = None

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.reason is not None:
            out["reason"] = self.reason
        if self.scope is not None:
            out["scope"] = self.scope
        if self.monitor_id is not None:
            out["monitor_id"] = self.monitor_id
        if self.contract_id is not None:
            out["contract_id"] = self.contract_id
        if self.start_at is not None:
            out["start_at"] = self.start_at
        if self.end_at is not None:
            out["end_at"] = self.end_at
        return out


@dataclass(slots=True)
class MonitorTemplateUpdateInput:
    """Partial update payload for PATCH /templates/{id}."""

    name: str | None = None
    description: str | None = None
    rules: list[JSONObject] | None = None
    channel_ids: list[int] | None = None
    parameters: list[JSONObject] | None = None

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.name is not None:
            out["name"] = self.name
        if self.description is not None:
            out["description"] = self.description
        if self.rules is not None:
            out["rules"] = _as_json_val(self.rules)
        if self.channel_ids is not None:
            out["channel_ids"] = _as_json_val(self.channel_ids)
        if self.parameters is not None:
            out["parameters"] = _as_json_val(self.parameters)
        return out


@dataclass(slots=True)
class FederatedIngestInput:
    """Request payload for POST /ingest."""

    alert: Alert
    origin: str
    hop_count: int | None = None

    def to_dict(self) -> JSONObject:
        out: JSONObject = {
            "alert": self.alert.to_dict(),
            "origin": self.origin,
        }
        if self.hop_count is not None:
            out["hop_count"] = self.hop_count
        return out


@dataclass(slots=True)
class FederatedIngestResult:
    """Response payload for POST /ingest."""

    outcome: str = ""
    alert_id: int | None = None
    origin: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> FederatedIngestResult:
        return cls(
            outcome=_as_str(data.get("outcome")),
            alert_id=_as_opt_int(data.get("alert_id")),
            origin=_as_opt_str(data.get("origin")),
        )


@dataclass(slots=True)
class TriggerEvaluationInput:
    """Request payload for POST /triggers."""

    monitor_id: Int64
    event: str | None = None
    source: str | None = None

    def to_dict(self) -> JSONObject:
        out: JSONObject = {"monitor_id": parse_wide_int(self.monitor_id)}
        if self.event is not None:
            out["event"] = self.event
        if self.source is not None:
            out["source"] = self.source
        return out


@dataclass(slots=True)
class TriggerEvaluationResult:
    """Response payload for POST /triggers."""

    alert_id: int | None = None
    note: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> TriggerEvaluationResult:
        return cls(
            alert_id=_as_opt_int(data.get("alert_id")),
            note=_as_opt_str(data.get("note")),
        )


@dataclass(slots=True)
class RotateTokenInput:
    """Optional rotation parameters for POST /tokens/{id}/rotate."""

    name: str | None = None
    scopes: list[str] | None = None
    overlap: str | None = None
    expires_in: str | None = None

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.name is not None:
            out["name"] = self.name
        if self.scopes is not None:
            out["scopes"] = _as_json_val(self.scopes)
        if self.overlap is not None:
            out["overlap"] = self.overlap
        if self.expires_in is not None:
            out["expires_in"] = self.expires_in
        return out


@dataclass(slots=True)
class ChannelSigningKey:
    """Channel webhook signing key metadata (secret omitted on read)."""

    id: str | None = None
    key_id: str | None = None
    status: str | None = None
    created_at: str | None = None
    retired_at: str | None = None
    revoked_at: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> ChannelSigningKey:
        return cls(
            id=_as_opt_str(data.get("id")),
            key_id=_as_opt_str(data.get("key_id")),
            status=_as_opt_str(data.get("status")),
            created_at=_as_opt_str(data.get("created_at")),
            retired_at=_as_opt_str(data.get("retired_at")),
            revoked_at=_as_opt_str(data.get("revoked_at")),
        )


@dataclass(slots=True)
class RotatedChannelSigningKey:
    """Created/rotated channel signing key response (secret returned once)."""

    id: str | None = None
    key_id: str | None = None
    secret: str | None = None
    created_at: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> RotatedChannelSigningKey:
        return cls(
            id=_as_opt_str(data.get("id")),
            key_id=_as_opt_str(data.get("key_id")),
            secret=_as_opt_str(data.get("secret")),
            created_at=_as_opt_str(data.get("created_at")),
        )


@dataclass(slots=True)
class PersistedGraphQLOperation:
    """Persisted GraphQL operation entry."""

    id: str | None = None
    name: str | None = None
    query: str | None = None
    created_at: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> PersistedGraphQLOperation:
        return cls(
            id=_as_opt_str(data.get("id")),
            name=_as_opt_str(data.get("name")),
            query=_as_opt_str(data.get("query")),
            created_at=_as_opt_str(data.get("created_at")),
        )


class SoroBeaconClient:
    """Generated Python client for the SoroBeacon HTTP API (/api/v1)."""

    _base_url: str
    _token: str | None
    _timeout_seconds: int
    _default_headers: dict[str, str]

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        *,
        token: str | None = None,
        timeout_seconds: int = 30,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        trimmed = base_url.strip().rstrip("/")
        if not trimmed.startswith(("http://", "https://")):
            raise ValueError(f"base_url must start with http:// or https://: {base_url!r}")
        if timeout_seconds <= 0:
            raise ValueError("timeout_seconds must be positive")
        self._base_url = trimmed if trimmed.endswith("/api/v1") else f"{trimmed}/api/v1"
        self._token = token.strip() if (token is not None and token.strip() != "") else None
        self._timeout_seconds = timeout_seconds
        self._default_headers = dict(headers) if headers is not None else {}

    @property
    def base_url(self) -> str:
        return self._base_url

    def _build_url(self, path: str, query: Mapping[str, object] | None = None) -> str:
        url = f"{self._base_url}{path}"
        if not query:
            return url
        pairs: list[tuple[str, str]] = []
        for k, v in query.items():
            if v is None:
                continue
            if isinstance(v, bool):
                pairs.append((k, "true" if v else "false"))
            elif isinstance(v, (int, Decimal)):
                pairs.append((k, format_wide_int(v)))
            else:
                pairs.append((k, str(v)))
        if not pairs:
            return url
        return f"{url}?{urllib.parse.urlencode(pairs)}"

    def _build_headers(
        self,
        extra: Mapping[str, str] | None = None,
        has_json_body: bool = False,
        accept: str = "application/json",
        content_type: str | None = None,
    ) -> dict[str, str]:
        headers: dict[str, str] = {
            "Accept": accept,
            **self._default_headers,
        }
        if extra is not None:
            headers.update(extra)
        if content_type is not None:
            headers["Content-Type"] = content_type
        elif has_json_body and "Content-Type" not in headers:
            headers["Content-Type"] = "application/json"
        if self._token is not None and "Authorization" not in headers:
            headers["Authorization"] = f"Bearer {self._token}"
        return headers

    def _request_raw(
        self,
        method: str,
        path: str,
        *,
        query: Mapping[str, object] | None = None,
        body: object = None,
        raw_body: str | None = None,
        accept: str = "application/json",
        content_type: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> tuple[int, dict[str, str], str]:
        has_json_body = body is not None
        req_headers = self._build_headers(
            extra=headers,
            has_json_body=has_json_body,
            accept=accept,
            content_type=content_type,
        )
        data_bytes: bytes | None = None
        if has_json_body:
            data_bytes = dumps_json_with_wide_ints(body).encode("utf-8")
        elif raw_body is not None:
            data_bytes = raw_body.encode("utf-8")

        url = self._build_url(path, query)
        req = urllib.request.Request(url=url, data=data_bytes, headers=req_headers, method=method)

        try:
            with urllib.request.urlopen(req, timeout=self._timeout_seconds) as resp:
                status = int(resp.status)
                resp_headers = {k.lower(): v for k, v in resp.headers.items()}
                text = resp.read().decode("utf-8", errors="replace")
                return status, resp_headers, text
        except urllib.error.HTTPError as http_err:
            status = int(http_err.code)
            err_headers = {k.lower(): v for k, v in http_err.headers.items()} if http_err.headers else {}
            err_text = http_err.read().decode("utf-8", errors="replace") if http_err.fp else ""
            raise self._decode_error(status, err_headers, err_text) from None
        except urllib.error.URLError as url_err:
            raise SoroBeaconError(f"request to {url} failed: {url_err.reason}") from url_err

    def _request_json(
        self,
        method: str,
        path: str,
        *,
        query: Mapping[str, object] | None = None,
        body: object = None,
        raw_body: str | None = None,
        content_type: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> JSONValue:
        status, _, text = self._request_raw(
            method,
            path,
            query=query,
            body=body,
            raw_body=raw_body,
            content_type=content_type,
            headers=headers,
        )
        if status == 204 or text.strip() == "":
            return None
        return parse_json_preserving_wide_ints(text)

    @staticmethod
    def _decode_error(
        status: int,
        headers: Mapping[str, str],
        raw_text: str,
    ) -> SoroBeaconAPIError:
        message = raw_text.strip() or http.client.responses.get(status, f"HTTP {status}")
        code = ""
        request_id = headers.get("x-request-id")
        details: list[ErrorDetail] = []
        retry_after_hdr = headers.get("retry-after")
        retry_after: int | None = None
        if retry_after_hdr is not None and retry_after_hdr.strip().isdigit():
            retry_after = int(retry_after_hdr.strip(), 10)

        if raw_text.strip():
            try:
                parsed = parse_json_preserving_wide_ints(raw_text)
                if isinstance(parsed, Mapping):
                    err_msg = parsed.get("error")
                    if isinstance(err_msg, str) and err_msg != "":
                        message = err_msg
                    code_val = parsed.get("code")
                    if isinstance(code_val, str):
                        code = code_val
                    req_id_val = parsed.get("request_id")
                    if isinstance(req_id_val, str) and req_id_val != "":
                        request_id = req_id_val
                    details_val = parsed.get("details")
                    if isinstance(details_val, list):
                        details = [
                            ErrorDetail.from_dict(item)
                            for item in details_val
                            if isinstance(item, Mapping)
                        ]
            except (ValueError, TypeError):
                message = http.client.responses.get(status, f"HTTP {status}")

        return _build_api_error(
            status,
            message,
            code=code,
            request_id=request_id,
            details=details,
            retry_after=retry_after,
        )

    @staticmethod
    def _encode_id(value: Int64) -> str:
        return urllib.parse.quote(format_wide_int(value), safe="")

    # -------------------------------------------------------------------------
    # Monitors
    # -------------------------------------------------------------------------

    def list_monitors(
        self,
        *,
        q: str | None = None,
        enabled: bool | None = None,
        tag: str | None = None,
        network: str | None = None,
        sort: str | None = None,
        limit: int | None = None,
        cursor: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorPage:
        res = self._request_json(
            "GET",
            "/monitors",
            query={
                "q": q,
                "enabled": enabled,
                "tag": tag,
                "network": network,
                "sort": sort,
                "limit": limit,
                "cursor": cursor,
            },
            headers=headers,
        )
        return MonitorPage.from_dict(_as_json_obj(res))

    def iter_monitors(
        self,
        *,
        q: str | None = None,
        enabled: bool | None = None,
        tag: str | None = None,
        network: str | None = None,
        sort: str | None = None,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> CursorIterator[Monitor]:
        """Return an iterator that transparently pages through GET /monitors."""

        def _fetch(cursor: str | None) -> tuple[list[Monitor], str | None]:
            page = self.list_monitors(
                q=q,
                enabled=enabled,
                tag=tag,
                network=network,
                sort=sort,
                limit=limit,
                cursor=cursor,
                headers=headers,
            )
            return page.monitors, page.next_cursor

        return CursorIterator(_fetch)

    def create_monitor(
        self,
        input_data: MonitorInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        res = self._request_json("POST", "/monitors", body=input_data, headers=headers)
        return Monitor.from_dict(_as_json_obj(res))

    def get_monitor(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        res = self._request_json(
            "GET",
            f"/monitors/{self._encode_id(monitor_id)}",
            headers=headers,
        )
        return Monitor.from_dict(_as_json_obj(res))

    def update_monitor(
        self,
        monitor_id: Int64,
        input_data: MonitorUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        res = self._request_json(
            "PATCH",
            f"/monitors/{self._encode_id(monitor_id)}",
            body=input_data,
            headers=headers,
        )
        return Monitor.from_dict(_as_json_obj(res))

    def delete_monitor(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "DELETE",
            f"/monitors/{self._encode_id(monitor_id)}",
            headers=headers,
        )

    def bulk_update_monitors(
        self,
        input_data: BulkMonitorInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> BulkMonitorResult:
        res = self._request_json("POST", "/monitors/bulk", body=input_data, headers=headers)
        return BulkMonitorResult.from_dict(_as_json_obj(res))

    def import_monitors(
        self,
        input_data: ImportInput | str,
        *,
        name: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> list[ImportRowResult]:
        if isinstance(input_data, str):
            res = self._request_json(
                "POST",
                "/monitors/import",
                query={"name": name},
                raw_body=input_data,
                content_type="text/csv",
                headers=headers,
            )
        else:
            res = self._request_json(
                "POST",
                "/monitors/import",
                query={"name": name},
                body=input_data,
                headers=headers,
            )
        obj = _as_json_obj(res)
        rows = obj.get("results")
        if isinstance(rows, list):
            return [ImportRowResult.from_dict(r) for r in rows if isinstance(r, Mapping)]
        return []

    def list_trash_monitors(
        self,
        *,
        q: str | None = None,
        enabled: bool | None = None,
        tag: str | None = None,
        network: str | None = None,
        sort: str | None = None,
        limit: int | None = None,
        cursor: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorPage:
        res = self._request_json(
            "GET",
            "/monitors/trash",
            query={
                "q": q,
                "enabled": enabled,
                "tag": tag,
                "network": network,
                "sort": sort,
                "limit": limit,
                "cursor": cursor,
            },
            headers=headers,
        )
        return MonitorPage.from_dict(_as_json_obj(res))

    def iter_trash_monitors(
        self,
        *,
        q: str | None = None,
        enabled: bool | None = None,
        tag: str | None = None,
        network: str | None = None,
        sort: str | None = None,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> CursorIterator[Monitor]:
        """Return an iterator that transparently pages through GET /monitors/trash."""

        def _fetch(cursor: str | None) -> tuple[list[Monitor], str | None]:
            page = self.list_trash_monitors(
                q=q,
                enabled=enabled,
                tag=tag,
                network=network,
                sort=sort,
                limit=limit,
                cursor=cursor,
                headers=headers,
            )
            return page.monitors, page.next_cursor

        return CursorIterator(_fetch)

    def soft_delete_monitor(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "POST",
            f"/monitors/{self._encode_id(monitor_id)}/soft-delete",
            headers=headers,
        )

    def restore_monitor(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        res = self._request_json(
            "POST",
            f"/monitors/{self._encode_id(monitor_id)}/restore",
            headers=headers,
        )
        return Monitor.from_dict(_as_json_obj(res))

    def duplicate_monitor(
        self,
        monitor_id: Int64,
        *,
        name: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        body: JSONObject = {"name": name} if name is not None else {}
        res = self._request_json(
            "POST",
            f"/monitors/{self._encode_id(monitor_id)}/duplicate",
            body=body,
            headers=headers,
        )
        return Monitor.from_dict(_as_json_obj(res))

    def get_monitor_stats(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorStats:
        res = self._request_json(
            "GET",
            f"/monitors/{self._encode_id(monitor_id)}/stats",
            headers=headers,
        )
        return MonitorStats.from_dict(_as_json_obj(res))

    # -------------------------------------------------------------------------
    # Escalation Policies & Rules
    # -------------------------------------------------------------------------

    def get_monitor_escalation(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> EscalationPolicy:
        res = self._request_json(
            "GET",
            f"/monitors/{self._encode_id(monitor_id)}/escalation",
            headers=headers,
        )
        return EscalationPolicy.from_dict(_as_json_obj(res))

    def set_monitor_escalation(
        self,
        monitor_id: Int64,
        input_data: EscalationInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> EscalationPolicy:
        res = self._request_json(
            "PUT",
            f"/monitors/{self._encode_id(monitor_id)}/escalation",
            body=input_data,
            headers=headers,
        )
        return EscalationPolicy.from_dict(_as_json_obj(res))

    def delete_monitor_escalation(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "DELETE",
            f"/monitors/{self._encode_id(monitor_id)}/escalation",
            headers=headers,
        )

    def list_rules(
        self,
        monitor_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[Rule]:
        res = self._request_json(
            "GET",
            f"/monitors/{self._encode_id(monitor_id)}/rules",
            headers=headers,
        )
        if not isinstance(res, list):
            return []
        return [Rule.from_dict(item) for item in res if isinstance(item, Mapping)]

    def create_rule(
        self,
        monitor_id: Int64,
        input_data: RuleInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Rule:
        res = self._request_json(
            "POST",
            f"/monitors/{self._encode_id(monitor_id)}/rules",
            body=input_data,
            headers=headers,
        )
        return Rule.from_dict(_as_json_obj(res))

    def bulk_create_rules(
        self,
        monitor_id: Int64,
        rules: Sequence[RuleInput],
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[Rule]:
        res = self._request_json(
            "POST",
            f"/monitors/{self._encode_id(monitor_id)}/rules/bulk",
            body={"rules": [r.to_dict() for r in rules]},
            headers=headers,
        )
        obj = _as_json_obj(res)
        items = obj.get("rules")
        if isinstance(items, list):
            return [Rule.from_dict(item) for item in items if isinstance(item, Mapping)]
        return []

    def dry_run_rule(
        self,
        monitor_id: Int64,
        input_data: RuleInput,
        *,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> DryRunResult:
        res = self._request_json(
            "POST",
            f"/monitors/{self._encode_id(monitor_id)}/rules/dry-run",
            query={"limit": limit},
            body=input_data,
            headers=headers,
        )
        return DryRunResult.from_dict(_as_json_obj(res))

    def update_rule(
        self,
        monitor_id: Int64,
        rule_id: Int64,
        input_data: RuleUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Rule:
        res = self._request_json(
            "PATCH",
            f"/monitors/{self._encode_id(monitor_id)}/rules/{self._encode_id(rule_id)}",
            body=input_data,
            headers=headers,
        )
        return Rule.from_dict(_as_json_obj(res))

    def delete_rule(
        self,
        monitor_id: Int64,
        rule_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "DELETE",
            f"/monitors/{self._encode_id(monitor_id)}/rules/{self._encode_id(rule_id)}",
            headers=headers,
        )

    # -------------------------------------------------------------------------
    # Channels & Channel Signing Keys
    # -------------------------------------------------------------------------

    def list_channels(
        self,
        *,
        q: str | None = None,
        type: str | None = None,
        enabled: bool | None = None,
        sort: str | None = None,
        limit: int | None = None,
        cursor: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> ChannelPage:
        res = self._request_json(
            "GET",
            "/channels",
            query={
                "q": q,
                "type": type,
                "enabled": enabled,
                "sort": sort,
                "limit": limit,
                "cursor": cursor,
            },
            headers=headers,
        )
        return ChannelPage.from_dict(_as_json_obj(res))

    def iter_channels(
        self,
        *,
        q: str | None = None,
        type: str | None = None,
        enabled: bool | None = None,
        sort: str | None = None,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> CursorIterator[Channel]:
        """Return an iterator that transparently pages through GET /channels."""

        def _fetch(cursor: str | None) -> tuple[list[Channel], str | None]:
            page = self.list_channels(
                q=q,
                type=type,
                enabled=enabled,
                sort=sort,
                limit=limit,
                cursor=cursor,
                headers=headers,
            )
            return page.channels, page.next_cursor

        return CursorIterator(_fetch)

    def create_channel(
        self,
        input_data: ChannelInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Channel:
        res = self._request_json("POST", "/channels", body=input_data, headers=headers)
        return Channel.from_dict(_as_json_obj(res))

    def get_channel(
        self,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Channel:
        res = self._request_json(
            "GET",
            f"/channels/{self._encode_id(channel_id)}",
            headers=headers,
        )
        return Channel.from_dict(_as_json_obj(res))

    def update_channel(
        self,
        channel_id: Int64,
        input_data: ChannelUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Channel:
        res = self._request_json(
            "PATCH",
            f"/channels/{self._encode_id(channel_id)}",
            body=input_data,
            headers=headers,
        )
        return Channel.from_dict(_as_json_obj(res))

    def delete_channel(
        self,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "DELETE",
            f"/channels/{self._encode_id(channel_id)}",
            headers=headers,
        )

    def bulk_update_channels(
        self,
        ids: Sequence[Int64],
        enabled: bool,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> BulkMonitorResult:
        res = self._request_json(
            "POST",
            "/channels/bulk",
            body={"ids": [parse_wide_int(i) for i in ids], "enabled": enabled},
            headers=headers,
        )
        return BulkMonitorResult.from_dict(_as_json_obj(res))

    def get_channel_stats(
        self,
        channel_id: Int64,
        *,
        window: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> ChannelDeliveryStats:
        res = self._request_json(
            "GET",
            f"/channels/{self._encode_id(channel_id)}/stats",
            query={"window": window},
            headers=headers,
        )
        return ChannelDeliveryStats.from_dict(_as_json_obj(res))

    def duplicate_channel(
        self,
        channel_id: Int64,
        *,
        name: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> Channel:
        body: JSONObject = {"name": name} if name is not None else {}
        res = self._request_json(
            "POST",
            f"/channels/{self._encode_id(channel_id)}/duplicate",
            body=body,
            headers=headers,
        )
        return Channel.from_dict(_as_json_obj(res))

    def test_channel(
        self,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JSONObject:
        res = self._request_json(
            "POST",
            f"/channels/{self._encode_id(channel_id)}/test",
            headers=headers,
        )
        return _as_json_obj(res)

    def list_channel_keys(
        self,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[ChannelSigningKey]:
        res = self._request_json(
            "GET",
            f"/channels/{self._encode_id(channel_id)}/keys",
            headers=headers,
        )
        if isinstance(res, list):
            return [ChannelSigningKey.from_dict(k) for k in res if isinstance(k, Mapping)]
        obj = _as_json_obj(res)
        keys = obj.get("keys")
        if isinstance(keys, list):
            return [ChannelSigningKey.from_dict(k) for k in keys if isinstance(k, Mapping)]
        return []

    def rotate_channel_key(
        self,
        channel_id: Int64,
        *,
        overlap: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> RotatedChannelSigningKey:
        body: JSONObject = {"overlap": overlap} if overlap is not None else {}
        res = self._request_json(
            "POST",
            f"/channels/{self._encode_id(channel_id)}/keys/rotate",
            body=body,
            headers=headers,
        )
        return RotatedChannelSigningKey.from_dict(_as_json_obj(res))

    def retire_channel_key(
        self,
        channel_id: Int64,
        key_id: str,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        encoded_key = urllib.parse.quote(key_id, safe="")
        self._request_json(
            "POST",
            f"/channels/{self._encode_id(channel_id)}/keys/{encoded_key}/retire",
            headers=headers,
        )

    def revoke_channel_key(
        self,
        channel_id: Int64,
        key_id: str,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        encoded_key = urllib.parse.quote(key_id, safe="")
        self._request_json(
            "POST",
            f"/channels/{self._encode_id(channel_id)}/keys/{encoded_key}/revoke",
            headers=headers,
        )

    # -------------------------------------------------------------------------
    # Maintenance Windows & Inhibitions
    # -------------------------------------------------------------------------

    def list_maintenance_windows(
        self,
        *,
        active: bool | None = None,
        upcoming: bool | None = None,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> MaintenanceWindowPage:
        res = self._request_json(
            "GET",
            "/maintenance-windows",
            query={"active": active, "upcoming": upcoming, "limit": limit},
            headers=headers,
        )
        return MaintenanceWindowPage.from_dict(_as_json_obj(res))

    def create_maintenance_window(
        self,
        input_data: MaintenanceWindowInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MaintenanceWindow:
        res = self._request_json(
            "POST",
            "/maintenance-windows",
            body=input_data,
            headers=headers,
        )
        return MaintenanceWindow.from_dict(_as_json_obj(res))

    def get_maintenance_window(
        self,
        window_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MaintenanceWindow:
        res = self._request_json(
            "GET",
            f"/maintenance-windows/{self._encode_id(window_id)}",
            headers=headers,
        )
        return MaintenanceWindow.from_dict(_as_json_obj(res))

    def update_maintenance_window(
        self,
        window_id: Int64,
        input_data: MaintenanceWindowUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MaintenanceWindow:
        res = self._request_json(
            "PATCH",
            f"/maintenance-windows/{self._encode_id(window_id)}",
            body=input_data,
            headers=headers,
        )
        return MaintenanceWindow.from_dict(_as_json_obj(res))

    def delete_maintenance_window(
        self,
        window_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "DELETE",
            f"/maintenance-windows/{self._encode_id(window_id)}",
            headers=headers,
        )

    def list_inhibitions(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[Inhibition]:
        res = self._request_json("GET", "/inhibitions", headers=headers)
        obj = _as_json_obj(res)
        items = obj.get("inhibitions")
        if isinstance(items, list):
            return [Inhibition.from_dict(i) for i in items if isinstance(i, Mapping)]
        return []

    def create_inhibition(
        self,
        input_data: InhibitionInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Inhibition:
        res = self._request_json("POST", "/inhibitions", body=input_data, headers=headers)
        return Inhibition.from_dict(_as_json_obj(res))

    def delete_inhibition(
        self,
        source_id: Int64,
        target_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "DELETE",
            f"/inhibitions/{self._encode_id(source_id)}/{self._encode_id(target_id)}",
            headers=headers,
        )

    # -------------------------------------------------------------------------
    # Alerts, Streaming, Exports & Deliveries
    # -------------------------------------------------------------------------

    def list_alerts(
        self,
        *,
        monitor_id: Int64 | None = None,
        rule_id: Int64 | None = None,
        contract_id: str | None = None,
        q: str | None = None,
        sort: str | None = None,
        from_time: str | None = None,
        to_time: str | None = None,
        network: str | None = None,
        limit: int | None = None,
        cursor: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> AlertPage:
        res = self._request_json(
            "GET",
            "/alerts",
            query={
                "monitor_id": monitor_id,
                "rule_id": rule_id,
                "contract_id": contract_id,
                "q": q,
                "sort": sort,
                "from": from_time,
                "to": to_time,
                "network": network,
                "limit": limit,
                "cursor": cursor,
            },
            headers=headers,
        )
        return AlertPage.from_dict(_as_json_obj(res))

    def iter_alerts(
        self,
        *,
        monitor_id: Int64 | None = None,
        rule_id: Int64 | None = None,
        contract_id: str | None = None,
        q: str | None = None,
        sort: str | None = None,
        from_time: str | None = None,
        to_time: str | None = None,
        network: str | None = None,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> CursorIterator[Alert]:
        """Return an iterator that transparently pages through GET /alerts."""

        def _fetch(cursor: str | None) -> tuple[list[Alert], str | None]:
            page = self.list_alerts(
                monitor_id=monitor_id,
                rule_id=rule_id,
                contract_id=contract_id,
                q=q,
                sort=sort,
                from_time=from_time,
                to_time=to_time,
                network=network,
                limit=limit,
                cursor=cursor,
                headers=headers,
            )
            return page.alerts, page.next_cursor

        return CursorIterator(_fetch)

    def stream_alerts(
        self,
        *,
        monitor_id: Int64 | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> Iterator[Alert]:
        """Connect to GET /alerts/stream (SSE) and yield decoded Alert objects."""
        _, _, text = self._request_raw(
            "GET",
            "/alerts/stream",
            query={"monitor_id": monitor_id},
            accept="text/event-stream",
            headers=headers,
        )
        for block in text.split("\n\n"):
            data_lines = [
                line[5:].lstrip()
                for line in block.splitlines()
                if line.startswith("data:")
            ]
            if data_lines:
                parsed = parse_json_preserving_wide_ints("\n".join(data_lines))
                if isinstance(parsed, Mapping):
                    yield Alert.from_dict(parsed)

    def export_alerts_csv(
        self,
        *,
        monitor_id: Int64 | None = None,
        rule_id: Int64 | None = None,
        contract_id: str | None = None,
        q: str | None = None,
        from_time: str | None = None,
        to_time: str | None = None,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> str:
        _, _, text = self._request_raw(
            "GET",
            "/alerts.csv",
            query={
                "monitor_id": monitor_id,
                "rule_id": rule_id,
                "contract_id": contract_id,
                "q": q,
                "from": from_time,
                "to": to_time,
                "limit": limit,
            },
            accept="text/csv",
            headers=headers,
        )
        return text

    def export_alerts_ndjson(
        self,
        *,
        monitor_id: Int64 | None = None,
        rule_id: Int64 | None = None,
        contract_id: str | None = None,
        q: str | None = None,
        from_time: str | None = None,
        to_time: str | None = None,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> list[Alert]:
        _, _, text = self._request_raw(
            "GET",
            "/alerts/export",
            query={
                "monitor_id": monitor_id,
                "rule_id": rule_id,
                "contract_id": contract_id,
                "q": q,
                "from": from_time,
                "to": to_time,
                "limit": limit,
            },
            accept="application/x-ndjson",
            headers=headers,
        )
        alerts: list[Alert] = []
        for line in text.splitlines():
            stripped = line.strip()
            if stripped:
                parsed = parse_json_preserving_wide_ints(stripped)
                if isinstance(parsed, Mapping):
                    alerts.append(Alert.from_dict(parsed))
        return alerts

    def acknowledge_alert(
        self,
        alert_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Alert:
        res = self._request_json(
            "POST",
            f"/alerts/{self._encode_id(alert_id)}/acknowledge",
            headers=headers,
        )
        return Alert.from_dict(_as_json_obj(res))

    def list_alert_deliveries(
        self,
        alert_id: Int64,
        *,
        status: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> list[DeliveryAttempt]:
        res = self._request_json(
            "GET",
            f"/alerts/{self._encode_id(alert_id)}/deliveries",
            query={"status": status},
            headers=headers,
        )
        if not isinstance(res, list):
            return []
        return [DeliveryAttempt.from_dict(d) for d in res if isinstance(d, Mapping)]

    def retry_alert_delivery(
        self,
        alert_id: Int64,
        channel_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> DeliveryAttempt:
        res = self._request_json(
            "POST",
            f"/alerts/{self._encode_id(alert_id)}/deliveries/{self._encode_id(channel_id)}/retry",
            headers=headers,
        )
        return DeliveryAttempt.from_dict(_as_json_obj(res))

    # -------------------------------------------------------------------------
    # Dead Letters, Federated Ingest & External Triggers
    # -------------------------------------------------------------------------

    def list_dead_letters(
        self,
        *,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> list[JSONObject]:
        res = self._request_json(
            "GET",
            "/dead-letters",
            query={"limit": limit},
            headers=headers,
        )
        obj = _as_json_obj(res)
        return _as_json_obj_list(obj.get("dead_letters"))

    def list_dead_letter_groups(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[JSONObject]:
        res = self._request_json("GET", "/dead-letters/groups", headers=headers)
        return _as_json_obj_list(res)

    def bulk_redrive_dead_letters(
        self,
        input_data: JSONObject | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JSONObject:
        res = self._request_json(
            "POST",
            "/dead-letters/bulk/redrive",
            body=input_data if input_data is not None else {},
            headers=headers,
        )
        return _as_json_obj(res)

    def bulk_discard_dead_letters(
        self,
        input_data: JSONObject | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JSONObject:
        res = self._request_json(
            "POST",
            "/dead-letters/bulk/discard",
            body=input_data if input_data is not None else {},
            headers=headers,
        )
        return _as_json_obj(res)

    def redrive_dead_letter(
        self,
        dead_letter_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JSONObject:
        res = self._request_json(
            "POST",
            f"/dead-letters/{self._encode_id(dead_letter_id)}/redrive",
            headers=headers,
        )
        return _as_json_obj(res)

    def discard_dead_letter(
        self,
        dead_letter_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "POST",
            f"/dead-letters/{self._encode_id(dead_letter_id)}/discard",
            headers=headers,
        )

    def ingest_federated_alert(
        self,
        input_data: FederatedIngestInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> FederatedIngestResult:
        res = self._request_json("POST", "/ingest", body=input_data, headers=headers)
        return FederatedIngestResult.from_dict(_as_json_obj(res))

    def trigger_evaluation(
        self,
        input_data: TriggerEvaluationInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> TriggerEvaluationResult:
        res = self._request_json("POST", "/triggers", body=input_data, headers=headers)
        return TriggerEvaluationResult.from_dict(_as_json_obj(res))

    # -------------------------------------------------------------------------
    # System Health, Poller, Version, Rule Types, Stats & Audit
    # -------------------------------------------------------------------------

    def get_health(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JSONObject:
        res = self._request_json("GET", "/health", headers=headers)
        return _as_json_obj(res)

    def get_livez(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JSONObject:
        res = self._request_json("GET", "/livez", headers=headers)
        return _as_json_obj(res)

    def get_readyz(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JSONObject:
        res = self._request_json("GET", "/readyz", headers=headers)
        return _as_json_obj(res)

    def get_poller_status(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> PollerStatus:
        res = self._request_json("GET", "/poller", headers=headers)
        return PollerStatus.from_dict(_as_json_obj(res))

    def get_version(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Version:
        res = self._request_json("GET", "/version", headers=headers)
        return Version.from_dict(_as_json_obj(res))

    def list_rule_types(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> RuleTypesResponse:
        res = self._request_json("GET", "/rule-types", headers=headers)
        return RuleTypesResponse.from_dict(_as_json_obj(res))

    def get_stats(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> JSONObject:
        res = self._request_json("GET", "/stats", headers=headers)
        return _as_json_obj(res)

    def get_daily_alert_counts(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> DailyAlertCountsResponse:
        res = self._request_json("GET", "/stats/alerts-daily", headers=headers)
        return DailyAlertCountsResponse.from_dict(_as_json_obj(res))

    def list_audit_logs(
        self,
        *,
        target_type: str | None = None,
        target_id: Int64 | None = None,
        actor: str | None = None,
        cursor: Int64 | None = None,
        from_time: str | None = None,
        to_time: str | None = None,
        limit: int | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> AuditPage:
        res = self._request_json(
            "GET",
            "/audit",
            query={
                "target_type": target_type,
                "target_id": target_id,
                "actor": actor,
                "cursor": cursor,
                "from": from_time,
                "to": to_time,
                "limit": limit,
            },
            headers=headers,
        )
        return AuditPage.from_dict(_as_json_obj(res))

    def iter_audit_logs(
        self,
        *,
        target_type: str | None = None,
        target_id: Int64 | None = None,
        actor: str | None = None,
        from_time: str | None = None,
        to_time: str | None = None,
        limit: int = 50,
        headers: Mapping[str, str] | None = None,
    ) -> CursorIterator[AuditEntry]:
        """Return an iterator that transparently pages through GET /audit."""

        def _fetch(cursor: str | None) -> tuple[list[AuditEntry], str | None]:
            cursor_int = parse_wide_int(cursor) if cursor is not None else None
            page = self.list_audit_logs(
                target_type=target_type,
                target_id=target_id,
                actor=actor,
                cursor=cursor_int,
                from_time=from_time,
                to_time=to_time,
                limit=limit,
                headers=headers,
            )
            entries = page.entries
            if len(entries) < limit or not entries:
                return entries, None
            return entries, str(entries[-1].id)

        return CursorIterator(_fetch)

    # -------------------------------------------------------------------------
    # Monitor Templates
    # -------------------------------------------------------------------------

    def list_templates(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[MonitorTemplate]:
        res = self._request_json("GET", "/templates", headers=headers)
        obj = _as_json_obj(res)
        items = obj.get("templates")
        if isinstance(items, list):
            return [MonitorTemplate.from_dict(t) for t in items if isinstance(t, Mapping)]
        return []

    def create_template(
        self,
        input_data: MonitorTemplateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorTemplate:
        res = self._request_json("POST", "/templates", body=input_data, headers=headers)
        return MonitorTemplate.from_dict(_as_json_obj(res))

    def get_template(
        self,
        template_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorTemplate:
        res = self._request_json(
            "GET",
            f"/templates/{self._encode_id(template_id)}",
            headers=headers,
        )
        return MonitorTemplate.from_dict(_as_json_obj(res))

    def update_template(
        self,
        template_id: Int64,
        input_data: MonitorTemplateUpdateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> MonitorTemplate:
        res = self._request_json(
            "PATCH",
            f"/templates/{self._encode_id(template_id)}",
            body=input_data,
            headers=headers,
        )
        return MonitorTemplate.from_dict(_as_json_obj(res))

    def delete_template(
        self,
        template_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "DELETE",
            f"/templates/{self._encode_id(template_id)}",
            headers=headers,
        )

    def instantiate_template(
        self,
        template_id: Int64,
        input_data: InstantiateInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> Monitor:
        res = self._request_json(
            "POST",
            f"/templates/{self._encode_id(template_id)}/instantiate",
            body=input_data,
            headers=headers,
        )
        return Monitor.from_dict(_as_json_obj(res))

    def bulk_instantiate_template(
        self,
        template_id: Int64,
        instances: Sequence[InstantiateInput],
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[JSONObject]:
        res = self._request_json(
            "POST",
            f"/templates/{self._encode_id(template_id)}/instantiate/bulk",
            body={"instances": [inst.to_dict() for inst in instances]},
            headers=headers,
        )
        obj = _as_json_obj(res)
        return _as_json_obj_list(obj.get("created"))

    # -------------------------------------------------------------------------
    # Scoped API Tokens & Persisted GraphQL Operations
    # -------------------------------------------------------------------------

    def list_tokens(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[APIToken]:
        res = self._request_json("GET", "/tokens", headers=headers)
        obj = _as_json_obj(res)
        items = obj.get("tokens")
        if isinstance(items, list):
            return [APIToken.from_dict(t) for t in items if isinstance(t, Mapping)]
        return []

    def create_token(
        self,
        input_data: TokenInput,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> CreatedAPIToken:
        res = self._request_json("POST", "/tokens", body=input_data, headers=headers)
        return CreatedAPIToken.from_dict(_as_json_obj(res))

    def get_token(
        self,
        token_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> APIToken:
        res = self._request_json(
            "GET",
            f"/tokens/{self._encode_id(token_id)}",
            headers=headers,
        )
        return APIToken.from_dict(_as_json_obj(res))

    def revoke_token(
        self,
        token_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        self._request_json(
            "POST",
            f"/tokens/{self._encode_id(token_id)}/revoke",
            headers=headers,
        )

    def rotate_token(
        self,
        token_id: Int64,
        input_data: RotateTokenInput | None = None,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> CreatedAPIToken:
        res = self._request_json(
            "POST",
            f"/tokens/{self._encode_id(token_id)}/rotate",
            body=input_data if input_data is not None else {},
            headers=headers,
        )
        return CreatedAPIToken.from_dict(_as_json_obj(res))

    def abandon_token_rotation(
        self,
        token_id: Int64,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> APIToken | None:
        res = self._request_json(
            "POST",
            f"/tokens/{self._encode_id(token_id)}/abandon",
            headers=headers,
        )
        if res is None:
            return None
        return APIToken.from_dict(_as_json_obj(res))

    def list_graphql_operations(
        self,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> list[PersistedGraphQLOperation]:
        res = self._request_json("GET", "/graphql/operations", headers=headers)
        if isinstance(res, list):
            return [PersistedGraphQLOperation.from_dict(op) for op in res if isinstance(op, Mapping)]
        obj = _as_json_obj(res)
        items = obj.get("operations")
        if isinstance(items, list):
            return [PersistedGraphQLOperation.from_dict(op) for op in items if isinstance(op, Mapping)]
        return []

    def register_graphql_operation(
        self,
        *,
        query: str,
        id: str | None = None,
        name: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> PersistedGraphQLOperation:
        body: JSONObject = {"query": query}
        if id is not None:
            body["id"] = id
        if name is not None:
            body["name"] = name
        res = self._request_json("POST", "/graphql/operations", body=body, headers=headers)
        return PersistedGraphQLOperation.from_dict(_as_json_obj(res))

    def get_graphql_operation(
        self,
        operation_id: str,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> PersistedGraphQLOperation:
        encoded = urllib.parse.quote(operation_id, safe="")
        res = self._request_json(
            "GET",
            f"/graphql/operations/{encoded}",
            headers=headers,
        )
        return PersistedGraphQLOperation.from_dict(_as_json_obj(res))

    def delete_graphql_operation(
        self,
        operation_id: str,
        *,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        encoded = urllib.parse.quote(operation_id, safe="")
        self._request_json(
            "DELETE",
            f"/graphql/operations/{encoded}",
            headers=headers,
        )
