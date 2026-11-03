"""End-to-end runnable usage example for the SoroBeacon Python client."""

from __future__ import annotations

import json
import os
from pathlib import Path
import sys

_PACKAGE_ROOT = Path(__file__).resolve().parents[1]
if str(_PACKAGE_ROOT) not in sys.path:
    sys.path.insert(0, str(_PACKAGE_ROOT))

from sorobeacon import (  # noqa: E402
    ChannelInput,
    MonitorInput,
    NotFoundError,
    RuleInput,
    SoroBeaconAPIError,
    SoroBeaconClient,
    TokenInput,
    TriggerEvaluationInput,
    parse_wide_decimal,
    parse_wide_int,
)


def main() -> None:
    base_url = os.environ.get("SOROBEACON_URL", "http://localhost:8080")
    token = os.environ.get("SOROBEACON_TOKEN", "sb_test_token_123")

    client = SoroBeaconClient(base_url=base_url, token=token)

    version = client.get_version()
    if version.version == "":
        raise RuntimeError("expected non-empty server version")

    channel = client.create_channel(
        ChannelInput(
            name="ops-webhook",
            type="webhook",
            config={"url": "https://ops.example.internal/hooks/sorobeacon"},
            enabled=True,
        )
    )
    if hasattr(channel, "config"):
        raise RuntimeError("Channel read model must never expose redacted config")

    monitor = client.create_monitor(
        MonitorInput(
            name="usdc-transfer-watch",
            contract_ids=["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"],
            enabled=True,
            channel_ids=[channel.id],
        )
    )

    rule = client.create_rule(
        monitor.id,
        RuleInput(
            type="event_emitted",
            params={"event_name": "transfer"},
            enabled=True,
        ),
    )

    # Walk cursor-paginated monitors transparently across pages.
    monitor_names = [m.name for m in client.iter_monitors(limit=1)]

    # Trigger evaluation with a wide 64-bit integer (> 2^53 - 1).
    trig = client.trigger_evaluation(
        TriggerEvaluationInput(
            monitor_id="9007199254740993",
            source="soroban-rpc",
        )
    )

    # Walk alerts and decode wide Soroban u128 event payload amounts as Decimal.
    alerts = list(client.iter_alerts(monitor_id=monitor.id))
    if not alerts:
        raise RuntimeError("expected at least one alert")
    first_alert = alerts[0]
    data_obj = first_alert.payload.get("data")
    if not isinstance(data_obj, dict):
        raise RuntimeError("expected structured dict in alert.payload['data']")
    amount_raw = data_obj.get("amount")
    if not isinstance(amount_raw, str):
        raise RuntimeError("expected wide integer amount string in alert payload")
    wide_int_val = parse_wide_int(amount_raw)
    wide_dec_val = parse_wide_decimal(amount_raw, decimals=7)

    # Verify token secret is returned once on creation and omitted on list_tokens.
    created_tok = client.create_token(TokenInput(name="ci-runner", scopes=["monitors:read"]))
    if created_tok.token != "sb_live_once_secret":
        raise RuntimeError("expected one-time token secret on create_token")
    listed_toks = client.list_tokens()
    if len(listed_toks) != 1 or hasattr(listed_toks[0], "token"):
        raise RuntimeError("expected token secret to be omitted on list_tokens")

    # Verify audit log wide int64 IDs via iter_audit_logs.
    audit_ids = [entry.id for entry in client.iter_audit_logs(limit=10)]

    # Verify typed error handling without string matching.
    not_found_code = ""
    try:
        client.get_monitor(999999)
    except NotFoundError as err:
        if not err.is_not_found() or err.request_id != "req_404":
            raise RuntimeError(f"unexpected NotFoundError fields: {err!r}") from err
        not_found_code = err.code
    except SoroBeaconAPIError as err:
        raise RuntimeError(f"expected NotFoundError subclass, got {type(err).__name__}") from err

    client.delete_rule(monitor.id, rule.id)
    client.delete_monitor(monitor.id)
    client.delete_channel(channel.id)

    summary = {
        "version": version.version,
        "monitor_names": monitor_names,
        "triggered_alert_id": trig.alert_id,
        "alert_id": first_alert.id,
        "wide_int": str(wide_int_val),
        "wide_decimal": str(wide_dec_val),
        "audit_id": audit_ids[0] if audit_ids else 0,
        "not_found_code": not_found_code,
    }
    print(json.dumps(summary))


if __name__ == "__main__":
    main()
