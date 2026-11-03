# Code generated from internal/api/openapi.json (SoroBeacon API v1.0.0). DO NOT EDIT.
# Regenerate with: go generate ./clients/python/...

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation
from enum import Enum
from typing import Final, TypeAlias

SPEC_VERSION: Final[str] = "1.0.0"

# Wide integers (Soroban i128/u128/i256/u256 and 64-bit resource/ledger IDs)
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


class ErrorCode(str, Enum):
    """Stable machine-readable error codes returned in the API Error envelope."""

    NOT_FOUND = "NOT_FOUND"
    ALREADY_EXISTS = "ALREADY_EXISTS"
    INVALID_ARGUMENT = "INVALID_ARGUMENT"
    CONFLICT = "CONFLICT"
    UNAUTHENTICATED = "UNAUTHENTICATED"
    PERMISSION_DENIED = "PERMISSION_DENIED"
    RATE_LIMITED = "RATE_LIMITED"
    QUOTA_EXCEEDED = "QUOTA_EXCEEDED"
    PAYLOAD_TOO_LARGE = "PAYLOAD_TOO_LARGE"
    METHOD_NOT_ALLOWED = "METHOD_NOT_ALLOWED"
    NOT_IMPLEMENTED = "NOT_IMPLEMENTED"
    SERVICE_UNAVAILABLE = "SERVICE_UNAVAILABLE"
    INTERNAL_ERROR = "INTERNAL_ERROR"


class RuleTypeName(str, Enum):
    """Built-in rule evaluation types."""

    EVENT_EMITTED = "event_emitted"
    VALUE_THRESHOLD = "value_threshold"
    TOKEN_EVENT = "token_event"
    SELF_TRANSFER = "self_transfer"
    TIME_WINDOW = "time_window"
    ABSENCE_OF_EVENT = "absence_of_event"


class ChannelType(str, Enum):
    """Supported notification delivery channel types."""

    DISCORD = "discord"
    SLACK = "slack"
    TELEGRAM = "telegram"
    EMAIL = "email"
    WEBHOOK = "webhook"
    NTFY = "ntfy"


@dataclass(slots=True)
class ErrorDetail:
    field: str = ""
    reason: str = ""

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> ErrorDetail:
        return cls(
            field=_as_str(data.get("field")),
            reason=_as_str(data.get("reason")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["field"] = self.field
        out["reason"] = self.reason
        return out


@dataclass(slots=True)
class Error:
    error: str = ""
    code: str | None = None
    details: list[ErrorDetail] | None = None
    request_id: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> Error:
        return cls(
            error=_as_str(data.get("error")),
            code=_as_opt_str(data.get("code")),
            details=[ErrorDetail.from_dict(item) for item in _as_list(data.get("details")) if isinstance(item, Mapping)] if isinstance(data.get("details"), list) else None,
            request_id=_as_opt_str(data.get("request_id")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["error"] = self.error
        if self.code is not None:
            out["code"] = self.code
        if self.details is not None:
            out["details"] = [item.to_dict() for item in self.details]
        if self.request_id is not None:
            out["request_id"] = self.request_id
        return out


@dataclass(slots=True)
class EscalationStep:
    channel_ids: list[int] = field(default_factory=list)
    delay: str = ""
    position: int = 0

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> EscalationStep:
        return cls(
            channel_ids=_as_int_list(data.get("channel_ids")),
            delay=_as_str(data.get("delay")),
            position=_as_int(data.get("position")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["channel_ids"] = _as_json_val(self.channel_ids)
        out["delay"] = self.delay
        out["position"] = self.position
        return out


@dataclass(slots=True)
class EscalationInput:
    steps: list[EscalationStep] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> EscalationInput:
        return cls(
            steps=[EscalationStep.from_dict(item) for item in _as_list(data.get("steps")) if isinstance(item, Mapping)],
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["steps"] = [item.to_dict() for item in self.steps]
        return out


@dataclass(slots=True)
class EscalationPolicy:
    created_at: str = ""
    id: int = 0
    monitor_id: int = 0
    steps: list[EscalationStep] = field(default_factory=list)
    updated_at: str = ""

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> EscalationPolicy:
        return cls(
            created_at=_as_str(data.get("created_at")),
            id=_as_int(data.get("id")),
            monitor_id=_as_int(data.get("monitor_id")),
            steps=[EscalationStep.from_dict(item) for item in _as_list(data.get("steps")) if isinstance(item, Mapping)],
            updated_at=_as_str(data.get("updated_at")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["created_at"] = self.created_at
        out["id"] = self.id
        out["monitor_id"] = self.monitor_id
        out["steps"] = [item.to_dict() for item in self.steps]
        out["updated_at"] = self.updated_at
        return out


@dataclass(slots=True)
class RuleParameter:
    default: str = ""
    description: str = ""
    name: str = ""
    required: bool = False
    type: str = ""
    options: list[str] | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> RuleParameter:
        return cls(
            default=_as_str(data.get("default")),
            description=_as_str(data.get("description")),
            name=_as_str(data.get("name")),
            required=_as_bool(data.get("required")),
            type=_as_str(data.get("type")),
            options=_as_str_list(data.get("options")) if isinstance(data.get("options"), list) else None,
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["default"] = self.default
        out["description"] = self.description
        out["name"] = self.name
        out["required"] = self.required
        out["type"] = self.type
        if self.options is not None:
            out["options"] = _as_json_val(self.options)
        return out


@dataclass(slots=True)
class RuleType:
    parameters: list[RuleParameter] = field(default_factory=list)
    type: str = ""

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> RuleType:
        return cls(
            parameters=[RuleParameter.from_dict(item) for item in _as_list(data.get("parameters")) if isinstance(item, Mapping)],
            type=_as_str(data.get("type")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["parameters"] = [item.to_dict() for item in self.parameters]
        out["type"] = self.type
        return out


@dataclass(slots=True)
class RuleTypesResponse:
    rule_types: list[RuleType] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> RuleTypesResponse:
        return cls(
            rule_types=[RuleType.from_dict(item) for item in _as_list(data.get("rule_types")) if isinstance(item, Mapping)],
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["rule_types"] = [item.to_dict() for item in self.rule_types]
        return out


@dataclass(slots=True)
class RuleMatchCount:
    alerts: int | None = None
    rule_id: int | None = None
    type: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> RuleMatchCount:
        return cls(
            alerts=_as_opt_int(data.get("alerts")),
            rule_id=_as_opt_int(data.get("rule_id")),
            type=_as_opt_str(data.get("type")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.alerts is not None:
            out["alerts"] = self.alerts
        if self.rule_id is not None:
            out["rule_id"] = self.rule_id
        if self.type is not None:
            out["type"] = self.type
        return out


@dataclass(slots=True)
class MonitorStats:
    alerts: int = 0
    alerts_last_24h: int = 0
    alerts_last_7d: int = 0
    deliveries_failed: int = 0
    deliveries_succeeded: int = 0
    monitor_id: int = 0
    rules: list[RuleMatchCount] = field(default_factory=list)
    last_alert_at: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> MonitorStats:
        return cls(
            alerts=_as_int(data.get("alerts")),
            alerts_last_24h=_as_int(data.get("alerts_last_24h")),
            alerts_last_7d=_as_int(data.get("alerts_last_7d")),
            deliveries_failed=_as_int(data.get("deliveries_failed")),
            deliveries_succeeded=_as_int(data.get("deliveries_succeeded")),
            monitor_id=_as_int(data.get("monitor_id")),
            rules=[RuleMatchCount.from_dict(item) for item in _as_list(data.get("rules")) if isinstance(item, Mapping)],
            last_alert_at=_as_opt_str(data.get("last_alert_at")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["alerts"] = self.alerts
        out["alerts_last_24h"] = self.alerts_last_24h
        out["alerts_last_7d"] = self.alerts_last_7d
        out["deliveries_failed"] = self.deliveries_failed
        out["deliveries_succeeded"] = self.deliveries_succeeded
        out["monitor_id"] = self.monitor_id
        out["rules"] = [item.to_dict() for item in self.rules]
        if self.last_alert_at is not None:
            out["last_alert_at"] = self.last_alert_at
        return out


@dataclass(slots=True)
class Rule:
    enabled: bool = False
    id: int = 0
    monitor_id: int = 0
    params: JSONObject = field(default_factory=dict)
    type: str = ""

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> Rule:
        return cls(
            enabled=_as_bool(data.get("enabled")),
            id=_as_int(data.get("id")),
            monitor_id=_as_int(data.get("monitor_id")),
            params=_as_json_obj(data.get("params")),
            type=_as_str(data.get("type")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["enabled"] = self.enabled
        out["id"] = self.id
        out["monitor_id"] = self.monitor_id
        out["params"] = _as_json_val(self.params)
        out["type"] = self.type
        return out


@dataclass(slots=True)
class RuleInput:
    params: JSONObject = field(default_factory=dict)
    type: str = ""
    enabled: bool | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> RuleInput:
        return cls(
            params=_as_json_obj(data.get("params")),
            type=_as_str(data.get("type")),
            enabled=_as_opt_bool(data.get("enabled")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["params"] = _as_json_val(self.params)
        out["type"] = self.type
        if self.enabled is not None:
            out["enabled"] = self.enabled
        return out


@dataclass(slots=True)
class Monitor:
    contract_ids: list[str] = field(default_factory=list)
    enabled: bool = False
    id: int = 0
    name: str = ""
    channel_ids: list[int] | None = None
    network: str | None = None
    tags: list[str] | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> Monitor:
        return cls(
            contract_ids=_as_str_list(data.get("contract_ids")),
            enabled=_as_bool(data.get("enabled")),
            id=_as_int(data.get("id")),
            name=_as_str(data.get("name")),
            channel_ids=_as_int_list(data.get("channel_ids")) if isinstance(data.get("channel_ids"), list) else None,
            network=_as_opt_str(data.get("network")),
            tags=_as_str_list(data.get("tags")) if isinstance(data.get("tags"), list) else None,
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["contract_ids"] = _as_json_val(self.contract_ids)
        out["enabled"] = self.enabled
        out["id"] = self.id
        out["name"] = self.name
        if self.channel_ids is not None:
            out["channel_ids"] = _as_json_val(self.channel_ids)
        if self.network is not None:
            out["network"] = self.network
        if self.tags is not None:
            out["tags"] = _as_json_val(self.tags)
        return out


@dataclass(slots=True)
class MonitorInput:
    contract_ids: list[str] = field(default_factory=list)
    name: str = ""
    channel_ids: list[int] | None = None
    enabled: bool | None = None
    network: str | None = None
    tags: list[str] | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> MonitorInput:
        return cls(
            contract_ids=_as_str_list(data.get("contract_ids")),
            name=_as_str(data.get("name")),
            channel_ids=_as_int_list(data.get("channel_ids")) if isinstance(data.get("channel_ids"), list) else None,
            enabled=_as_opt_bool(data.get("enabled")),
            network=_as_opt_str(data.get("network")),
            tags=_as_str_list(data.get("tags")) if isinstance(data.get("tags"), list) else None,
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["contract_ids"] = _as_json_val(self.contract_ids)
        out["name"] = self.name
        if self.channel_ids is not None:
            out["channel_ids"] = _as_json_val(self.channel_ids)
        if self.enabled is not None:
            out["enabled"] = self.enabled
        if self.network is not None:
            out["network"] = self.network
        if self.tags is not None:
            out["tags"] = _as_json_val(self.tags)
        return out


@dataclass(slots=True)
class MonitorPage:
    monitors: list[Monitor] = field(default_factory=list)
    next_cursor: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> MonitorPage:
        return cls(
            monitors=[Monitor.from_dict(item) for item in _as_list(data.get("monitors")) if isinstance(item, Mapping)],
            next_cursor=_as_opt_str(data.get("next_cursor")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["monitors"] = [item.to_dict() for item in self.monitors]
        if self.next_cursor is not None:
            out["next_cursor"] = self.next_cursor
        return out


@dataclass(slots=True)
class Channel:
    " Note: channel configuration (`config`) is write-only on ChannelInput and is never returned by the server."

    enabled: bool = False
    id: int = 0
    name: str = ""
    type: str = ""
    consecutive_failures: int | None = None
    consecutive_permanent_failures: int | None = None
    disabled_at: str | None = None
    last_error: str | None = None
    last_error_at: str | None = None
    last_success_at: str | None = None
    timeout: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> Channel:
        return cls(
            enabled=_as_bool(data.get("enabled")),
            id=_as_int(data.get("id")),
            name=_as_str(data.get("name")),
            type=_as_str(data.get("type")),
            consecutive_failures=_as_opt_int(data.get("consecutive_failures")),
            consecutive_permanent_failures=_as_opt_int(data.get("consecutive_permanent_failures")),
            disabled_at=_as_opt_str(data.get("disabled_at")),
            last_error=_as_opt_str(data.get("last_error")),
            last_error_at=_as_opt_str(data.get("last_error_at")),
            last_success_at=_as_opt_str(data.get("last_success_at")),
            timeout=_as_opt_int(data.get("timeout")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["enabled"] = self.enabled
        out["id"] = self.id
        out["name"] = self.name
        out["type"] = self.type
        if self.consecutive_failures is not None:
            out["consecutive_failures"] = self.consecutive_failures
        if self.consecutive_permanent_failures is not None:
            out["consecutive_permanent_failures"] = self.consecutive_permanent_failures
        if self.disabled_at is not None:
            out["disabled_at"] = self.disabled_at
        if self.last_error is not None:
            out["last_error"] = self.last_error
        if self.last_error_at is not None:
            out["last_error_at"] = self.last_error_at
        if self.last_success_at is not None:
            out["last_success_at"] = self.last_success_at
        if self.timeout is not None:
            out["timeout"] = self.timeout
        return out


@dataclass(slots=True)
class ChannelInput:
    config: JSONObject = field(default_factory=dict)
    name: str = ""
    type: str = ""
    digest_mode: str | None = None
    digest_window_seconds: int | None = None
    enabled: bool | None = None
    timeout: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> ChannelInput:
        return cls(
            config=_as_json_obj(data.get("config")),
            name=_as_str(data.get("name")),
            type=_as_str(data.get("type")),
            digest_mode=_as_opt_str(data.get("digest_mode")),
            digest_window_seconds=_as_opt_int(data.get("digest_window_seconds")),
            enabled=_as_opt_bool(data.get("enabled")),
            timeout=_as_opt_int(data.get("timeout")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["config"] = _as_json_val(self.config)
        out["name"] = self.name
        out["type"] = self.type
        if self.digest_mode is not None:
            out["digest_mode"] = self.digest_mode
        if self.digest_window_seconds is not None:
            out["digest_window_seconds"] = self.digest_window_seconds
        if self.enabled is not None:
            out["enabled"] = self.enabled
        if self.timeout is not None:
            out["timeout"] = self.timeout
        return out


@dataclass(slots=True)
class ChannelPage:
    channels: list[Channel] = field(default_factory=list)
    next_cursor: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> ChannelPage:
        return cls(
            channels=[Channel.from_dict(item) for item in _as_list(data.get("channels")) if isinstance(item, Mapping)],
            next_cursor=_as_opt_str(data.get("next_cursor")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["channels"] = [item.to_dict() for item in self.channels]
        if self.next_cursor is not None:
            out["next_cursor"] = self.next_cursor
        return out


@dataclass(slots=True)
class Alert:
    created_at: str = ""
    event_id: str = ""
    event_name: str = ""
    id: int = 0
    monitor_id: int = 0
    payload: JSONObject = field(default_factory=dict)
    rule_id: int = 0
    federated: bool | None = None
    federation_origin: str | None = None
    monitor_name: str | None = None
    network: str | None = None
    source_monitor_id: int | None = None
    source_rule_id: int | None = None
    suppressed: bool | None = None
    suppression_reason: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> Alert:
        return cls(
            created_at=_as_str(data.get("created_at")),
            event_id=_as_str(data.get("event_id")),
            event_name=_as_str(data.get("event_name")),
            id=_as_int(data.get("id")),
            monitor_id=_as_int(data.get("monitor_id")),
            payload=_as_json_obj(data.get("payload")),
            rule_id=_as_int(data.get("rule_id")),
            federated=_as_opt_bool(data.get("federated")),
            federation_origin=_as_opt_str(data.get("federation_origin")),
            monitor_name=_as_opt_str(data.get("monitor_name")),
            network=_as_opt_str(data.get("network")),
            source_monitor_id=_as_opt_int(data.get("source_monitor_id")),
            source_rule_id=_as_opt_int(data.get("source_rule_id")),
            suppressed=_as_opt_bool(data.get("suppressed")),
            suppression_reason=_as_opt_str(data.get("suppression_reason")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["created_at"] = self.created_at
        out["event_id"] = self.event_id
        out["event_name"] = self.event_name
        out["id"] = self.id
        out["monitor_id"] = self.monitor_id
        out["payload"] = _as_json_val(self.payload)
        out["rule_id"] = self.rule_id
        if self.federated is not None:
            out["federated"] = self.federated
        if self.federation_origin is not None:
            out["federation_origin"] = self.federation_origin
        if self.monitor_name is not None:
            out["monitor_name"] = self.monitor_name
        if self.network is not None:
            out["network"] = self.network
        if self.source_monitor_id is not None:
            out["source_monitor_id"] = self.source_monitor_id
        if self.source_rule_id is not None:
            out["source_rule_id"] = self.source_rule_id
        if self.suppressed is not None:
            out["suppressed"] = self.suppressed
        if self.suppression_reason is not None:
            out["suppression_reason"] = self.suppression_reason
        return out


@dataclass(slots=True)
class AlertPage:
    alerts: list[Alert] = field(default_factory=list)
    next_cursor: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> AlertPage:
        return cls(
            alerts=[Alert.from_dict(item) for item in _as_list(data.get("alerts")) if isinstance(item, Mapping)],
            next_cursor=_as_opt_str(data.get("next_cursor")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["alerts"] = [item.to_dict() for item in self.alerts]
        if self.next_cursor is not None:
            out["next_cursor"] = self.next_cursor
        return out


@dataclass(slots=True)
class MaintenanceWindow:
    created_at: str = ""
    end_at: str = ""
    id: int = 0
    reason: str = ""
    scope: str = ""
    start_at: str = ""
    contract_id: str | None = None
    monitor_id: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> MaintenanceWindow:
        return cls(
            created_at=_as_str(data.get("created_at")),
            end_at=_as_str(data.get("end_at")),
            id=_as_int(data.get("id")),
            reason=_as_str(data.get("reason")),
            scope=_as_str(data.get("scope")),
            start_at=_as_str(data.get("start_at")),
            contract_id=_as_opt_str(data.get("contract_id")),
            monitor_id=_as_opt_int(data.get("monitor_id")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["created_at"] = self.created_at
        out["end_at"] = self.end_at
        out["id"] = self.id
        out["reason"] = self.reason
        out["scope"] = self.scope
        out["start_at"] = self.start_at
        if self.contract_id is not None:
            out["contract_id"] = self.contract_id
        if self.monitor_id is not None:
            out["monitor_id"] = self.monitor_id
        return out


@dataclass(slots=True)
class MaintenanceWindowInput:
    end_at: str = ""
    reason: str = ""
    scope: str = ""
    start_at: str = ""
    contract_id: str | None = None
    monitor_id: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> MaintenanceWindowInput:
        return cls(
            end_at=_as_str(data.get("end_at")),
            reason=_as_str(data.get("reason")),
            scope=_as_str(data.get("scope")),
            start_at=_as_str(data.get("start_at")),
            contract_id=_as_opt_str(data.get("contract_id")),
            monitor_id=_as_opt_int(data.get("monitor_id")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["end_at"] = self.end_at
        out["reason"] = self.reason
        out["scope"] = self.scope
        out["start_at"] = self.start_at
        if self.contract_id is not None:
            out["contract_id"] = self.contract_id
        if self.monitor_id is not None:
            out["monitor_id"] = self.monitor_id
        return out


@dataclass(slots=True)
class MaintenanceWindowPage:
    maintenance_windows: list[MaintenanceWindow] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> MaintenanceWindowPage:
        return cls(
            maintenance_windows=[MaintenanceWindow.from_dict(item) for item in _as_list(data.get("maintenance_windows")) if isinstance(item, Mapping)],
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["maintenance_windows"] = [item.to_dict() for item in self.maintenance_windows]
        return out


@dataclass(slots=True)
class MonitorTemplate:
    created_at: str = ""
    id: int = 0
    name: str = ""
    channel_ids: list[int] | None = None
    description: str | None = None
    parameters: list[JSONObject] | None = None
    rules: list[JSONObject] | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> MonitorTemplate:
        return cls(
            created_at=_as_str(data.get("created_at")),
            id=_as_int(data.get("id")),
            name=_as_str(data.get("name")),
            channel_ids=_as_int_list(data.get("channel_ids")) if isinstance(data.get("channel_ids"), list) else None,
            description=_as_opt_str(data.get("description")),
            parameters=_as_json_obj_list(data.get("parameters")) if isinstance(data.get("parameters"), list) else None,
            rules=_as_json_obj_list(data.get("rules")) if isinstance(data.get("rules"), list) else None,
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["created_at"] = self.created_at
        out["id"] = self.id
        out["name"] = self.name
        if self.channel_ids is not None:
            out["channel_ids"] = _as_json_val(self.channel_ids)
        if self.description is not None:
            out["description"] = self.description
        if self.parameters is not None:
            out["parameters"] = _as_json_val(self.parameters)
        if self.rules is not None:
            out["rules"] = _as_json_val(self.rules)
        return out


@dataclass(slots=True)
class MonitorTemplateInput:
    name: str = ""
    channel_ids: list[int] | None = None
    description: str | None = None
    parameters: list[JSONObject] | None = None
    rules: list[JSONObject] | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> MonitorTemplateInput:
        return cls(
            name=_as_str(data.get("name")),
            channel_ids=_as_int_list(data.get("channel_ids")) if isinstance(data.get("channel_ids"), list) else None,
            description=_as_opt_str(data.get("description")),
            parameters=_as_json_obj_list(data.get("parameters")) if isinstance(data.get("parameters"), list) else None,
            rules=_as_json_obj_list(data.get("rules")) if isinstance(data.get("rules"), list) else None,
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["name"] = self.name
        if self.channel_ids is not None:
            out["channel_ids"] = _as_json_val(self.channel_ids)
        if self.description is not None:
            out["description"] = self.description
        if self.parameters is not None:
            out["parameters"] = _as_json_val(self.parameters)
        if self.rules is not None:
            out["rules"] = _as_json_val(self.rules)
        return out


@dataclass(slots=True)
class APIToken:
    " Note: the plaintext token secret (`token`) is returned only once in CreatedAPIToken and is omitted on APIToken."

    created_at: str = ""
    id: int = 0
    name: str = ""
    prefix: str = ""
    scopes: list[str] = field(default_factory=list)
    expires_at: str | None = None
    last_used_at: str | None = None
    revoked_at: str | None = None
    rotated_at: str | None = None
    rotated_from_id: int | None = None
    successor_id: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> APIToken:
        return cls(
            created_at=_as_str(data.get("created_at")),
            id=_as_int(data.get("id")),
            name=_as_str(data.get("name")),
            prefix=_as_str(data.get("prefix")),
            scopes=_as_str_list(data.get("scopes")),
            expires_at=_as_opt_str(data.get("expires_at")),
            last_used_at=_as_opt_str(data.get("last_used_at")),
            revoked_at=_as_opt_str(data.get("revoked_at")),
            rotated_at=_as_opt_str(data.get("rotated_at")),
            rotated_from_id=_as_opt_int(data.get("rotated_from_id")),
            successor_id=_as_opt_int(data.get("successor_id")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["created_at"] = self.created_at
        out["id"] = self.id
        out["name"] = self.name
        out["prefix"] = self.prefix
        out["scopes"] = _as_json_val(self.scopes)
        if self.expires_at is not None:
            out["expires_at"] = self.expires_at
        if self.last_used_at is not None:
            out["last_used_at"] = self.last_used_at
        if self.revoked_at is not None:
            out["revoked_at"] = self.revoked_at
        if self.rotated_at is not None:
            out["rotated_at"] = self.rotated_at
        if self.rotated_from_id is not None:
            out["rotated_from_id"] = self.rotated_from_id
        if self.successor_id is not None:
            out["successor_id"] = self.successor_id
        return out


@dataclass(slots=True)
class BulkMonitorInput:
    enabled: bool = False
    ids: list[int] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> BulkMonitorInput:
        return cls(
            enabled=_as_bool(data.get("enabled")),
            ids=_as_int_list(data.get("ids")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["enabled"] = self.enabled
        out["ids"] = _as_json_val(self.ids)
        return out


@dataclass(slots=True)
class BulkMonitorResult:
    enabled: bool = False
    unknown_ids: list[int] = field(default_factory=list)
    updated: int = 0

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> BulkMonitorResult:
        return cls(
            enabled=_as_bool(data.get("enabled")),
            unknown_ids=_as_int_list(data.get("unknown_ids")),
            updated=_as_int(data.get("updated")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["enabled"] = self.enabled
        out["unknown_ids"] = _as_json_val(self.unknown_ids)
        out["updated"] = self.updated
        return out


@dataclass(slots=True)
class ChannelDeliveryStats:
    "One channel's delivery outcomes over the requested window. Attempts are counted, not unique alerts: a retry is another attempt."

    channel_id: int = 0
    failed: int = 0
    succeeded: int = 0
    total: int = 0
    window: str = ""
    last_failure_at: str | None = None
    last_success_at: str | None = None
    success_rate: Decimal | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> ChannelDeliveryStats:
        return cls(
            channel_id=_as_int(data.get("channel_id")),
            failed=_as_int(data.get("failed")),
            succeeded=_as_int(data.get("succeeded")),
            total=_as_int(data.get("total")),
            window=_as_str(data.get("window")),
            last_failure_at=_as_opt_str(data.get("last_failure_at")),
            last_success_at=_as_opt_str(data.get("last_success_at")),
            success_rate=_as_opt_decimal(data.get("success_rate")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["channel_id"] = self.channel_id
        out["failed"] = self.failed
        out["succeeded"] = self.succeeded
        out["total"] = self.total
        out["window"] = self.window
        if self.last_failure_at is not None:
            out["last_failure_at"] = self.last_failure_at
        if self.last_success_at is not None:
            out["last_success_at"] = self.last_success_at
        if self.success_rate is not None:
            out["success_rate"] = self.success_rate
        return out


@dataclass(slots=True)
class CreatedAPIToken:
    created_at: str = ""
    id: int = 0
    name: str = ""
    prefix: str = ""
    scopes: list[str] = field(default_factory=list)
    token: str = ""
    expires_at: str | None = None
    last_used_at: str | None = None
    revoked_at: str | None = None
    rotated_at: str | None = None
    rotated_from_id: int | None = None
    successor_id: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> CreatedAPIToken:
        return cls(
            created_at=_as_str(data.get("created_at")),
            id=_as_int(data.get("id")),
            name=_as_str(data.get("name")),
            prefix=_as_str(data.get("prefix")),
            scopes=_as_str_list(data.get("scopes")),
            token=_as_str(data.get("token")),
            expires_at=_as_opt_str(data.get("expires_at")),
            last_used_at=_as_opt_str(data.get("last_used_at")),
            revoked_at=_as_opt_str(data.get("revoked_at")),
            rotated_at=_as_opt_str(data.get("rotated_at")),
            rotated_from_id=_as_opt_int(data.get("rotated_from_id")),
            successor_id=_as_opt_int(data.get("successor_id")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["created_at"] = self.created_at
        out["id"] = self.id
        out["name"] = self.name
        out["prefix"] = self.prefix
        out["scopes"] = _as_json_val(self.scopes)
        out["token"] = self.token
        if self.expires_at is not None:
            out["expires_at"] = self.expires_at
        if self.last_used_at is not None:
            out["last_used_at"] = self.last_used_at
        if self.revoked_at is not None:
            out["revoked_at"] = self.revoked_at
        if self.rotated_at is not None:
            out["rotated_at"] = self.rotated_at
        if self.rotated_from_id is not None:
            out["rotated_from_id"] = self.rotated_from_id
        if self.successor_id is not None:
            out["successor_id"] = self.successor_id
        return out


@dataclass(slots=True)
class DeliveryAttempt:
    alert_id: int = 0
    attempted_at: str = ""
    channel_id: int = 0
    status: str = ""
    response_snippet: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> DeliveryAttempt:
        return cls(
            alert_id=_as_int(data.get("alert_id")),
            attempted_at=_as_str(data.get("attempted_at")),
            channel_id=_as_int(data.get("channel_id")),
            status=_as_str(data.get("status")),
            response_snippet=_as_opt_str(data.get("response_snippet")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["alert_id"] = self.alert_id
        out["attempted_at"] = self.attempted_at
        out["channel_id"] = self.channel_id
        out["status"] = self.status
        if self.response_snippet is not None:
            out["response_snippet"] = self.response_snippet
        return out


@dataclass(slots=True)
class DryRunResult:
    count: int = 0
    matches: list[JSONObject] = field(default_factory=list)
    note: str = ""
    total_evaluated: int = 0

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> DryRunResult:
        return cls(
            count=_as_int(data.get("count")),
            matches=_as_json_obj_list(data.get("matches")),
            note=_as_str(data.get("note")),
            total_evaluated=_as_int(data.get("total_evaluated")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["count"] = self.count
        out["matches"] = _as_json_val(self.matches)
        out["note"] = self.note
        out["total_evaluated"] = self.total_evaluated
        return out


@dataclass(slots=True)
class ImportInput:
    contract_ids: list[str] = field(default_factory=list)
    name: str = ""
    channel_ids: list[int] | None = None
    enabled: bool | None = None
    network: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> ImportInput:
        return cls(
            contract_ids=_as_str_list(data.get("contract_ids")),
            name=_as_str(data.get("name")),
            channel_ids=_as_int_list(data.get("channel_ids")) if isinstance(data.get("channel_ids"), list) else None,
            enabled=_as_opt_bool(data.get("enabled")),
            network=_as_opt_str(data.get("network")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["contract_ids"] = _as_json_val(self.contract_ids)
        out["name"] = self.name
        if self.channel_ids is not None:
            out["channel_ids"] = _as_json_val(self.channel_ids)
        if self.enabled is not None:
            out["enabled"] = self.enabled
        if self.network is not None:
            out["network"] = self.network
        return out


@dataclass(slots=True)
class ImportRowResult:
    contract_id: str | None = None
    error: str | None = None
    monitor_id: int | None = None
    row: int | None = None
    status: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> ImportRowResult:
        return cls(
            contract_id=_as_opt_str(data.get("contract_id")),
            error=_as_opt_str(data.get("error")),
            monitor_id=_as_opt_int(data.get("monitor_id")),
            row=_as_opt_int(data.get("row")),
            status=_as_opt_str(data.get("status")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        if self.contract_id is not None:
            out["contract_id"] = self.contract_id
        if self.error is not None:
            out["error"] = self.error
        if self.monitor_id is not None:
            out["monitor_id"] = self.monitor_id
        if self.row is not None:
            out["row"] = self.row
        if self.status is not None:
            out["status"] = self.status
        return out


@dataclass(slots=True)
class Inhibition:
    created_at: str = ""
    firing_window_seconds: int = 0
    source_rule_id: int = 0
    target_rule_id: int = 0

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> Inhibition:
        return cls(
            created_at=_as_str(data.get("created_at")),
            firing_window_seconds=_as_int(data.get("firing_window_seconds")),
            source_rule_id=_as_int(data.get("source_rule_id")),
            target_rule_id=_as_int(data.get("target_rule_id")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["created_at"] = self.created_at
        out["firing_window_seconds"] = self.firing_window_seconds
        out["source_rule_id"] = self.source_rule_id
        out["target_rule_id"] = self.target_rule_id
        return out


@dataclass(slots=True)
class InhibitionInput:
    source_rule_id: int = 0
    target_rule_id: int = 0
    firing_window_seconds: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> InhibitionInput:
        return cls(
            source_rule_id=_as_int(data.get("source_rule_id")),
            target_rule_id=_as_int(data.get("target_rule_id")),
            firing_window_seconds=_as_opt_int(data.get("firing_window_seconds")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["source_rule_id"] = self.source_rule_id
        out["target_rule_id"] = self.target_rule_id
        if self.firing_window_seconds is not None:
            out["firing_window_seconds"] = self.firing_window_seconds
        return out


@dataclass(slots=True)
class InstantiateInput:
    contract_ids: list[str] = field(default_factory=list)
    name: str = ""
    network: str | None = None
    parameters: JSONObject | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> InstantiateInput:
        return cls(
            contract_ids=_as_str_list(data.get("contract_ids")),
            name=_as_str(data.get("name")),
            network=_as_opt_str(data.get("network")),
            parameters=_as_opt_json_obj(data.get("parameters")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["contract_ids"] = _as_json_val(self.contract_ids)
        out["name"] = self.name
        if self.network is not None:
            out["network"] = self.network
        if self.parameters is not None:
            out["parameters"] = _as_json_val(self.parameters)
        return out


@dataclass(slots=True)
class PollerStatus:
    backing_off: bool = False
    status: str = ""
    last_processed_ledger: int | None = None
    last_successful_poll: str | None = None
    latest_chain_ledger: int | None = None
    ledger_lag: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> PollerStatus:
        return cls(
            backing_off=_as_bool(data.get("backing_off")),
            status=_as_str(data.get("status")),
            last_processed_ledger=_as_opt_int(data.get("last_processed_ledger")),
            last_successful_poll=_as_opt_str(data.get("last_successful_poll")),
            latest_chain_ledger=_as_opt_int(data.get("latest_chain_ledger")),
            ledger_lag=_as_opt_int(data.get("ledger_lag")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["backing_off"] = self.backing_off
        out["status"] = self.status
        if self.last_processed_ledger is not None:
            out["last_processed_ledger"] = self.last_processed_ledger
        if self.last_successful_poll is not None:
            out["last_successful_poll"] = self.last_successful_poll
        if self.latest_chain_ledger is not None:
            out["latest_chain_ledger"] = self.latest_chain_ledger
        if self.ledger_lag is not None:
            out["ledger_lag"] = self.ledger_lag
        return out


@dataclass(slots=True)
class TokenInput:
    name: str = ""
    scopes: list[str] = field(default_factory=list)
    expires_in: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> TokenInput:
        return cls(
            name=_as_str(data.get("name")),
            scopes=_as_str_list(data.get("scopes")),
            expires_in=_as_opt_str(data.get("expires_in")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["name"] = self.name
        out["scopes"] = _as_json_val(self.scopes)
        if self.expires_in is not None:
            out["expires_in"] = self.expires_in
        return out


@dataclass(slots=True)
class Version:
    build_date: str = ""
    commit: str = ""
    version: str = ""

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> Version:
        return cls(
            build_date=_as_str(data.get("build_date")),
            commit=_as_str(data.get("commit")),
            version=_as_str(data.get("version")),
        )

    def to_dict(self) -> JSONObject:
        out: JSONObject = {}
        out["build_date"] = self.build_date
        out["commit"] = self.commit
        out["version"] = self.version
        return out


OPENAPI_OPERATIONS: Final[tuple[tuple[str, str, str, str], ...]] = (
    ("GET", "/monitors", "list_monitors", "List monitors"),
    ("POST", "/monitors", "create_monitor", "Create a monitor"),
    ("POST", "/monitors/bulk", "bulk_update_monitors", "Bulk enable or disable monitors"),
    ("POST", "/monitors/import", "import_monitors", "Bulk import contracts from a file"),
    ("GET", "/monitors/trash", "list_trash_monitors", "List deleted monitors (trash)"),
    ("GET", "/monitors/{id}", "get_monitor", "Fetch one monitor"),
    ("PATCH", "/monitors/{id}", "update_monitor", "Update a monitor"),
    ("DELETE", "/monitors/{id}", "delete_monitor", "Delete a monitor"),
    ("POST", "/monitors/{id}/soft-delete", "soft_delete_monitor", "Soft-delete a monitor"),
    ("POST", "/monitors/{id}/restore", "restore_monitor", "Restore a soft-deleted monitor"),
    ("POST", "/monitors/{id}/duplicate", "duplicate_monitor", "Duplicate a monitor"),
    ("GET", "/monitors/{id}/stats", "get_monitor_stats", "Per-monitor statistics"),
    ("GET", "/monitors/{id}/escalation", "get_monitor_escalation", "Get a monitor's escalation policy"),
    ("PUT", "/monitors/{id}/escalation", "set_monitor_escalation", "Create or replace a monitor's escalation policy"),
    ("DELETE", "/monitors/{id}/escalation", "delete_monitor_escalation", "Delete a monitor's escalation policy"),
    ("GET", "/monitors/{id}/rules", "list_rules", "List a monitor's rules"),
    ("POST", "/monitors/{id}/rules", "create_rule", "Add a rule to a monitor"),
    ("POST", "/monitors/{id}/rules/bulk", "bulk_create_rules", "Create several rules in one request"),
    ("POST", "/monitors/{id}/rules/dry-run", "dry_run_rule", "Dry-run a rule against historical events"),
    ("PATCH", "/monitors/{id}/rules/{ruleID}", "update_rule", "Update a rule"),
    ("DELETE", "/monitors/{id}/rules/{ruleID}", "delete_rule", "Delete a rule"),
    ("GET", "/channels", "list_channels", "List channels"),
    ("POST", "/channels", "create_channel", "Create a notification channel"),
    ("POST", "/channels/bulk", "bulk_update_channels", "Bulk enable or disable channels"),
    ("GET", "/channels/{id}", "get_channel", "Fetch one channel"),
    ("PATCH", "/channels/{id}", "update_channel", "Update a channel"),
    ("DELETE", "/channels/{id}", "delete_channel", "Delete a channel"),
    ("GET", "/channels/{id}/stats", "get_channel_stats", "Per-channel delivery statistics"),
    ("POST", "/channels/{id}/duplicate", "duplicate_channel", "Duplicate a channel"),
    ("POST", "/channels/{id}/test", "test_channel", "Send a test notification through a channel"),
    ("GET", "/channels/{id}/keys", "list_channel_keys", "List signing keys for a channel"),
    ("POST", "/channels/{id}/keys/rotate", "rotate_channel_key", "Rotate channel signing key with overlap"),
    ("POST", "/channels/{id}/keys/{keyID}/retire", "retire_channel_key", "Retire a channel signing key"),
    ("POST", "/channels/{id}/keys/{keyID}/revoke", "revoke_channel_key", "Revoke a channel signing key immediately"),
    ("GET", "/maintenance-windows", "list_maintenance_windows", "List maintenance windows"),
    ("POST", "/maintenance-windows", "create_maintenance_window", "Create a maintenance window"),
    ("GET", "/maintenance-windows/{id}", "get_maintenance_window", "Fetch one maintenance window"),
    ("PATCH", "/maintenance-windows/{id}", "update_maintenance_window", "Update a maintenance window"),
    ("DELETE", "/maintenance-windows/{id}", "delete_maintenance_window", "Delete a maintenance window"),
    ("GET", "/inhibitions", "list_inhibitions", "List inhibition rules"),
    ("POST", "/inhibitions", "create_inhibition", "Create an inhibition rule"),
    ("DELETE", "/inhibitions/{sourceID}/{targetID}", "delete_inhibition", "Delete an inhibition rule"),
    ("GET", "/alerts", "list_alerts", "List alerts"),
    ("GET", "/alerts/stream", "stream_alerts", "Stream alerts as they are created (Server-Sent Events)"),
    ("GET", "/alerts.csv", "export_alerts_csv", "Export alerts as CSV"),
    ("GET", "/alerts/export", "export_alerts_ndjson", "Export alerts as NDJSON"),
    ("POST", "/alerts/{id}/acknowledge", "acknowledge_alert", "Acknowledge an alert"),
    ("GET", "/alerts/{id}/deliveries", "list_alert_deliveries", "List one alert's delivery attempts"),
    ("POST", "/alerts/{id}/deliveries/{channelID}/retry", "retry_alert_delivery", "Retry a failed delivery"),
    ("GET", "/dead-letters", "list_dead_letters", "List dead-letter queue records"),
    ("GET", "/dead-letters/groups", "list_dead_letter_groups", "Roll up dead letters into groups by channel and cause"),
    ("POST", "/dead-letters/bulk/redrive", "bulk_redrive_dead_letters", "Re-drive a batch of dead letters matching filter or IDs"),
    ("POST", "/dead-letters/bulk/discard", "bulk_discard_dead_letters", "Discard a batch of dead letters matching filter or IDs"),
    ("POST", "/dead-letters/{id}/redrive", "redrive_dead_letter", "Re-drive a dead-letter delivery"),
    ("POST", "/dead-letters/{id}/discard", "discard_dead_letter", "Discard one dead letter from the backlog"),
    ("POST", "/ingest", "ingest_federated_alert", "Receive a federated alert from another SoroBeacon instance"),
    ("POST", "/triggers", "trigger_evaluation", "Trigger alert evaluation from an external event"),
    ("GET", "/health", "get_health", "Combined health report"),
    ("GET", "/livez", "get_livez", "Liveness probe"),
    ("GET", "/readyz", "get_readyz", "Readiness probe"),
    ("GET", "/poller", "get_poller_status", "Poller position and lag"),
    ("GET", "/version", "get_version", "Build information"),
    ("GET", "/rule-types", "list_rule_types", "List rule types and parameter schemas"),
    ("GET", "/stats", "get_stats", "Service statistics"),
    ("GET", "/stats/alerts-daily", "get_daily_alert_counts", "Daily alert counts for the last 30 UTC days"),
    ("GET", "/audit", "list_audit_logs", "Audit log of configuration changes"),
    ("GET", "/templates", "list_templates", "List monitor templates"),
    ("POST", "/templates", "create_template", "Create a monitor template"),
    ("GET", "/templates/{id}", "get_template", "Get a monitor template"),
    ("PATCH", "/templates/{id}", "update_template", "Update a monitor template"),
    ("DELETE", "/templates/{id}", "delete_template", "Delete a monitor template"),
    ("POST", "/templates/{id}/instantiate", "instantiate_template", "Create a monitor from a template"),
    ("POST", "/templates/{id}/instantiate/bulk", "bulk_instantiate_template", "Bulk create monitors from a template"),
    ("GET", "/tokens", "list_tokens", "List API tokens"),
    ("POST", "/tokens", "create_token", "Mint a scoped API token"),
    ("GET", "/tokens/{id}", "get_token", "Get an API token by ID"),
    ("POST", "/tokens/{id}/revoke", "revoke_token", "Revoke an API token"),
    ("POST", "/tokens/{id}/rotate", "rotate_token", "Rotate an API token with overlap"),
    ("POST", "/tokens/{id}/abandon", "abandon_token_rotation", "Abandon an in-progress token rotation"),
    ("GET", "/graphql/operations", "list_graphql_operations", "List registered persisted GraphQL operations"),
    ("POST", "/graphql/operations", "register_graphql_operation", "Register a persisted GraphQL operation"),
    ("GET", "/graphql/operations/{id}", "get_graphql_operation", "Get a registered persisted GraphQL operation"),
    ("DELETE", "/graphql/operations/{id}", "delete_graphql_operation", "Delete a registered persisted GraphQL operation"),
)
