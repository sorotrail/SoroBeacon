"""Runnable usage example for the generated SoroBeacon Python client."""

from __future__ import annotations

import os
import sys
from pathlib import Path

# Allow running `python examples/usage.py` directly from `clients/python`
# without a prior `pip install`.
_PKG_ROOT = Path(__file__).resolve().parent.parent
if str(_PKG_ROOT) not in sys.path:
    sys.path.insert(0, str(_PKG_ROOT))

from sorobeacon import (  # noqa: E402
    Alert,
    Channel,
    ChannelInput,
    Monitor,
    SoroBeaconAPIError,
    SoroBeaconClient,
    to_decimal,
    to_int,
)

CONTRACT_USDC = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
CONTRACT_DEX = "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"


def run_usage_example(base_url: str, token: str = "dev-token") -> None:
    """Exercise channel creation, monitor/rule setup, paging, wide ints, and errors."""
    client = SoroBeaconClient(base_url=base_url, token=token)

    # 1. Create a notification channel. ChannelInput requires `config` on write,
    #    while the returned Channel read type omits `config` because the server
    #    redacts secrets on read.
    channel_input: ChannelInput = {
        "name": "ops-webhook",
        "type": "webhook",
        "config": {
            "url": "https://hooks.example.com/sorobeacon",
        },
        "enabled": True,
    }
    channel: Channel = client.create_channel(channel_input)
    if "config" in channel:
        raise RuntimeError("expected channel.config to be redacted on read")

    # 2. Create monitors and attach an evaluation rule.
    m1: Monitor = client.create_monitor(
        {
            "name": "usdc-transfer-watch",
            "contract_ids": [CONTRACT_USDC],
            "enabled": True,
            "channel_ids": [channel["id"]],
            "tags": ["treasury", "usdc"],
        }
    )
    m2: Monitor = client.create_monitor(
        {
            "name": "dex-swap-watch",
            "contract_ids": [CONTRACT_DEX],
            "enabled": True,
        }
    )

    rule = client.create_rule(
        m1["id"],
        {
            "type": "event_emitted",
            "params": {"event_name": "transfer"},
            "enabled": True,
        },
    )

    # 3. Iterate across pages transparently using the generator pagination helper.
    seen_monitors: list[Monitor] = list(client.paginate_monitors({"limit": 1}))
    if len(seen_monitors) < 2:
        raise RuntimeError(
            f"expected paginate_monitors to walk at least 2 monitors, got {len(seen_monitors)}"
        )

    # 4. Walk alerts and inspect structured JSON payloads and wide 64-bit/128-bit
    #    integers without IEEE-754 float64 precision loss.
    seen_alerts: list[Alert] = []
    for alert in client.paginate_alerts({"monitor_id": m1["id"], "limit": 1}):
        seen_alerts.append(alert)
        alert_id_int = to_int(alert["id"])
        alert_id_dec = to_decimal(alert["id"])
        if alert_id_int <= 0 or alert_id_dec <= 0:
            raise RuntimeError(f"expected positive alert id, got {alert['id']!r}")
        if isinstance(alert["id"], float):
            raise RuntimeError("wide integer id must never be decoded as float")
        payload = alert["payload"]
        if not isinstance(payload, dict):
            raise RuntimeError("expected alert.payload to be a structured JSON dict")

    # 5. Exercise typed non-2xx error handling and taxonomy helpers.
    caught_not_found = False
    try:
        client.get_monitor(999999)
    except SoroBeaconAPIError as err:
        if SoroBeaconAPIError.is_error(err, "NOT_FOUND") and err.is_not_found():
            caught_not_found = True
        else:
            raise
    if not caught_not_found:
        raise RuntimeError(
            "expected get_monitor(999999) to raise SoroBeaconAPIError (NOT_FOUND)"
        )

    # 6. Query version and clean up created resources.
    version = client.get_version()
    if not isinstance(version.get("version"), str):
        raise RuntimeError("expected version['version'] string")

    client.delete_rule(m1["id"], rule["id"])
    client.delete_monitor(m2["id"])
    client.delete_monitor(m1["id"])
    client.delete_channel(channel["id"])


if __name__ == "__main__":
    env_url = os.environ.get("SOROBEACON_URL")
    if env_url:
        run_usage_example(env_url, os.environ.get("SOROBEACON_TOKEN", "dev-token"))
