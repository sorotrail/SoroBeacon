"""Unit, strict type-annotation, pagination, wide-int, and HTTP tests for sorobeacon."""

from __future__ import annotations

import ast
from collections.abc import Callable
from decimal import Decimal
import http.server
import json
from pathlib import Path
import shutil
import subprocess
import sys
import threading
import unittest
import urllib.parse

_PACKAGE_ROOT = Path(__file__).resolve().parents[1]
if str(_PACKAGE_ROOT) not in sys.path:
    sys.path.insert(0, str(_PACKAGE_ROOT))

import sorobeacon  # noqa: E402
from sorobeacon import (  # noqa: E402
    APIToken,
    Alert,
    AlertPayload,
    AlreadyExistsError,
    BadRequestError,
    Channel,
    ChannelInput,
    ChannelUpdateInput,
    ConflictError,
    CreatedAPIToken,
    ErrorCode,
    I128_MAX,
    I128_MIN,
    I256_MAX,
    I256_MIN,
    InternalServerError,
    MonitorInput,
    NotFoundError,
    OPENAPI_OPERATIONS,
    PayloadTooLargeError,
    PermissionDeniedError,
    QuotaExceededError,
    RateLimitedError,
    SPEC_VERSION,
    ServiceUnavailableError,
    SoroBeaconAPIError,
    SoroBeaconClient,
    SoroBeaconError,
    U128_MAX,
    U256_MAX,
    UnauthenticatedError,
    dumps_json_with_wide_ints,
    format_wide_int,
    parse_json_preserving_wide_ints,
    parse_wide_decimal,
    parse_wide_int,
)


class _MockRouteHandler(http.server.BaseHTTPRequestHandler):
    routes: dict[
        tuple[str, str],
        Callable[[dict[str, list[str]], bytes, dict[str, str]], tuple[int, dict[str, str], bytes]],
    ] = {}

    def log_message(self, format: str, *args: object) -> None:
        return

    def _dispatch(self, method: str) -> None:
        parsed = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length > 0 else b""
        req_headers = {k.lower(): v for k, v in self.headers.items()}
        handler = self.routes.get((method, parsed.path))
        if handler is None:
            payload = json.dumps(
                {"error": "resource not found", "code": "NOT_FOUND"}
            ).encode("utf-8")
            self.send_response(404)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return

        status, resp_headers, resp_body = handler(query, body, req_headers)
        self.send_response(status)
        for k, v in resp_headers.items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(resp_body)))
        self.end_headers()
        if resp_body:
            self.wfile.write(resp_body)

    def do_GET(self) -> None:
        self._dispatch("GET")

    def do_POST(self) -> None:
        self._dispatch("POST")

    def do_PUT(self) -> None:
        self._dispatch("PUT")

    def do_PATCH(self) -> None:
        self._dispatch("PATCH")

    def do_DELETE(self) -> None:
        self._dispatch("DELETE")


class TestTypeAnnotationsAndSpecParity(unittest.TestCase):
    """Verify full type-hint coverage, zero Any/float annotations, and OpenAPI parity."""

    def test_spec_metadata_and_operation_bindings(self) -> None:
        self.assertTrue(len(SPEC_VERSION) > 0)
        self.assertEqual(len(OPENAPI_OPERATIONS), 83)
        for name in sorobeacon.__all__:
            self.assertTrue(hasattr(sorobeacon, name), f"missing export {name}")
        for _method, _path, py_method, _summary in OPENAPI_OPERATIONS:
            self.assertTrue(
                hasattr(SoroBeaconClient, py_method),
                f"SoroBeaconClient missing method {py_method} for {_method} {_path}",
            )

    def test_every_function_and_method_has_complete_annotations_no_any_or_float(self) -> None:
        py_files = sorted(
            list((_PACKAGE_ROOT / "sorobeacon").glob("*.py"))
            + list((_PACKAGE_ROOT / "examples").glob("*.py"))
            + list((_PACKAGE_ROOT / "tests").glob("*.py"))
        )
        self.assertGreaterEqual(len(py_files), 5)

        for path in py_files:
            source = path.read_text(encoding="utf-8")
            tree = ast.parse(source, filename=str(path))
            for node in ast.walk(tree):
                if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    self.assertIsNotNone(
                        node.returns,
                        f"{path.name}:{node.lineno} function {node.name} missing return annotation",
                    )
                    all_args = (
                        list(node.args.posonlyargs)
                        + list(node.args.args)
                        + list(node.args.kwonlyargs)
                    )
                    if node.args.vararg is not None:
                        all_args.append(node.args.vararg)
                    if node.args.kwarg is not None:
                        all_args.append(node.args.kwarg)
                    for idx, arg in enumerate(all_args):
                        if idx == 0 and arg.arg in ("self", "cls"):
                            continue
                        self.assertIsNotNone(
                            arg.annotation,
                            f"{path.name}:{node.lineno} arg '{arg.arg}' in {node.name} missing type annotation",
                        )
                if isinstance(node, ast.Name) and node.id == "Any":
                    self.fail(f"{path.name}:{node.lineno} uses forbidden typing.Any")
                if isinstance(node, ast.Name) and node.id == "float":
                    self.fail(f"{path.name}:{node.lineno} uses forbidden float type")

    def test_mypy_strict_when_installed(self) -> None:
        mypy_bin = shutil.which("mypy")
        if mypy_bin is None:
            return
        proc = subprocess.run(
            [
                mypy_bin,
                "--config-file",
                str(_PACKAGE_ROOT / "pyproject.toml"),
                str(_PACKAGE_ROOT / "sorobeacon"),
                str(_PACKAGE_ROOT / "examples"),
                str(_PACKAGE_ROOT / "tests"),
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(
            proc.returncode,
            0,
            f"mypy --strict failed:\n{proc.stdout}\n{proc.stderr}",
        )


class TestWideIntegersAndStructuredJSON(unittest.TestCase):
    """Verify i128/u128/i256/u256 and int64 preservation without float truncation."""

    def test_parse_wide_int_boundaries(self) -> None:
        for bound in (I128_MIN, I128_MAX, U128_MAX, I256_MIN, I256_MAX, U256_MAX):
            self.assertEqual(parse_wide_int(str(bound)), bound)
            self.assertEqual(parse_wide_int(bound), bound)
            self.assertEqual(parse_wide_int(Decimal(bound)), bound)
            self.assertEqual(format_wide_int(bound), str(bound))

    def test_parse_wide_int_rejects_float_and_bool(self) -> None:
        bad_float: object = json.loads("1.5")
        with self.assertRaises(TypeError):
            parse_wide_int(bad_float)  # type: ignore[arg-type]
        with self.assertRaises(TypeError):
            parse_wide_int(True)
        with self.assertRaises(ValueError):
            parse_wide_int(Decimal("12.34"))
        with self.assertRaises(ValueError):
            parse_wide_int("")
        with self.assertRaises(ValueError):
            parse_wide_int("not-a-number")
        with self.assertRaises(ValueError):
            parse_wide_int(U256_MAX + 1)
        with self.assertRaises(ValueError):
            parse_wide_int(I256_MIN - 1)

    def test_parse_wide_decimal_scaling_and_rejection(self) -> None:
        raw_u128 = "340282366920938463463374607431768211455"
        self.assertEqual(
            parse_wide_decimal(raw_u128, decimals=7),
            Decimal("34028236692093846346337460743176.8211455"),
        )
        self.assertEqual(
            parse_wide_decimal(raw_u128, decimals=18),
            Decimal("340282366920938463463.374607431768211455"),
        )
        bad_float: object = json.loads("3.14")
        with self.assertRaises(TypeError):
            parse_wide_decimal(bad_float)  # type: ignore[arg-type]
        with self.assertRaises(TypeError):
            parse_wide_decimal(False)
        with self.assertRaises(ValueError):
            parse_wide_decimal("100", decimals=-1)

    def test_json_roundtrip_preserves_wide_integers_and_decimals(self) -> None:
        raw = '{"id":9007199254740993,"neg":-9007199254740995,"ratio":0.999999999999999999,"payload":{"amount":"340282366920938463463374607431768211455"}}'
        parsed = parse_json_preserving_wide_ints(raw)
        self.assertIsInstance(parsed, dict)
        if not isinstance(parsed, dict):
            self.fail("expected dict")
        self.assertEqual(parsed["id"], 9007199254740993)
        self.assertEqual(parsed["neg"], -9007199254740995)
        self.assertEqual(parsed["ratio"], Decimal("0.999999999999999999"))
        reserialized = dumps_json_with_wide_ints({"id": Decimal("9007199254740993")})
        self.assertEqual(reserialized, '{"id":9007199254740993}')

    def test_channel_config_and_token_secret_redacted_on_read_models(self) -> None:
        ch_input = ChannelInput(
            name="ops-slack",
            type="slack",
            config={"webhook_url": "https://hooks.slack.com/services/T000/SECRET"},
            enabled=True,
        )
        self.assertEqual(
            ch_input.config.get("webhook_url"),
            "https://hooks.slack.com/services/T000/SECRET",
        )
        self.assertIn("config", ch_input.to_dict())
        ch_update = ChannelUpdateInput(config={"webhook_url": "https://hooks.slack.com/services/T000/NEW"})
        self.assertIn("config", ch_update.to_dict())

        # Even if a rogue server sends "config" on a Channel response, Channel.from_dict
        # never retains or exposes it.
        ch = Channel.from_dict(
            {
                "id": 1,
                "name": "ops-slack",
                "type": "slack",
                "enabled": True,
                "config": {"webhook_url": "https://hooks.slack.com/services/T000/SECRET"},
            }
        )
        self.assertFalse(hasattr(ch, "config"))
        self.assertNotIn("config", ch.to_dict())
        self.assertNotIn("SECRET", dumps_json_with_wide_ints(ch))

        created_tok = CreatedAPIToken.from_dict(
            {
                "id": 5,
                "name": "deploy",
                "prefix": "sb_live_",
                "scopes": ["monitors:write"],
                "created_at": "2026-03-01T00:00:00Z",
                "token": "sb_live_secret_once",
            }
        )
        self.assertEqual(created_tok.token, "sb_live_secret_once")

        listed_tok = APIToken.from_dict(
            {
                "id": 5,
                "name": "deploy",
                "prefix": "sb_live_",
                "scopes": ["monitors:write"],
                "created_at": "2026-03-01T00:00:00Z",
                "token": "sb_live_secret_once",
            }
        )
        self.assertFalse(hasattr(listed_tok, "token"))
        self.assertNotIn("token", listed_tok.to_dict())

    def test_decoded_alert_payload_is_structured_json(self) -> None:
        alert = Alert.from_dict(
            {
                "id": 9007199254740995,
                "monitor_id": 10,
                "rule_id": 20,
                "event_id": "0009007199254740993-0000000001",
                "event_name": "transfer",
                "payload": {
                    "contract_id": "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
                    "event_name": "transfer",
                    "ledger": 9007199254740993,
                    "topics": ["transfer", "GABC"],
                    "data": {"amount": "340282366920938463463374607431768211455"},
                },
                "created_at": "2026-03-01T00:02:00Z",
            }
        )
        self.assertIsInstance(alert.payload, dict)
        structured = AlertPayload.from_dict(alert.payload)
        self.assertEqual(structured.event_name, "transfer")
        self.assertEqual(structured.ledger, 9007199254740993)


class TestClientPaginationAndErrors(unittest.TestCase):
    """Verify HTTP transport, cursor iterators, and typed error branching."""

    server: http.server.HTTPServer
    thread: threading.Thread
    base_url: str

    @classmethod
    def setUpClass(cls) -> None:
        cls.server = http.server.HTTPServer(("127.0.0.1", 0), _MockRouteHandler)
        port = cls.server.server_port
        cls.base_url = f"http://127.0.0.1:{port}"
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self) -> None:
        _MockRouteHandler.routes = {}

    def test_invalid_client_init_rejected(self) -> None:
        with self.assertRaises(ValueError):
            SoroBeaconClient("ftp://invalid")
        with self.assertRaises(ValueError):
            SoroBeaconClient("http://localhost:8080", timeout_seconds=0)

    def test_cursor_pagination_iterators(self) -> None:
        seen_cursors: list[str] = []

        def handle_monitors(
            query: dict[str, list[str]],
            _body: bytes,
            headers: dict[str, str],
        ) -> tuple[int, dict[str, str], bytes]:
            self.assertEqual(headers.get("authorization"), "Bearer sb_tok")
            cursor = query.get("cursor", [""])[0]
            seen_cursors.append(cursor)
            if cursor == "":
                data = {
                    "monitors": [
                        {"id": 1, "name": "m1", "contract_ids": ["CA"], "enabled": True},
                        {"id": 2, "name": "m2", "contract_ids": ["CB"], "enabled": True},
                    ],
                    "next_cursor": "cur_2",
                }
            else:
                data = {
                    "monitors": [
                        {"id": 3, "name": "m3", "contract_ids": ["CC"], "enabled": False},
                    ],
                    "next_cursor": "",
                }
            return 200, {"Content-Type": "application/json"}, json.dumps(data).encode("utf-8")

        _MockRouteHandler.routes[("GET", "/api/v1/monitors")] = handle_monitors
        client = SoroBeaconClient(self.base_url, token="sb_tok")
        items = list(client.iter_monitors(limit=2))
        self.assertEqual([m.name for m in items], ["m1", "m2", "m3"])
        self.assertEqual(seen_cursors, ["", "cur_2"])

    def test_typed_errors_branch_without_string_matching(self) -> None:
        cases: list[tuple[int, str, type[SoroBeaconAPIError], str]] = [
            (400, ErrorCode.INVALID_ARGUMENT.value, BadRequestError, "is_invalid_argument"),
            (401, ErrorCode.UNAUTHENTICATED.value, UnauthenticatedError, "is_unauthenticated"),
            (403, ErrorCode.PERMISSION_DENIED.value, PermissionDeniedError, "is_permission_denied"),
            (404, ErrorCode.NOT_FOUND.value, NotFoundError, "is_not_found"),
            (409, ErrorCode.ALREADY_EXISTS.value, AlreadyExistsError, "is_already_exists"),
            (409, ErrorCode.CONFLICT.value, ConflictError, "is_conflict"),
            (413, ErrorCode.PAYLOAD_TOO_LARGE.value, PayloadTooLargeError, "is_payload_too_large"),
            (429, ErrorCode.RATE_LIMITED.value, RateLimitedError, "is_rate_limited"),
            (429, ErrorCode.QUOTA_EXCEEDED.value, QuotaExceededError, "is_quota_exceeded"),
            (500, ErrorCode.INTERNAL_ERROR.value, InternalServerError, ""),
            (503, ErrorCode.SERVICE_UNAVAILABLE.value, ServiceUnavailableError, "is_service_unavailable"),
        ]

        client = SoroBeaconClient(self.base_url)
        for status, code, expected_cls, pred_name in cases:
            def handler(
                _q: dict[str, list[str]],
                _b: bytes,
                _h: dict[str, str],
                st: int = status,
                cd: str = code,
            ) -> tuple[int, dict[str, str], bytes]:
                body = json.dumps(
                    {
                        "error": f"error for {cd}",
                        "code": cd,
                        "request_id": "req_xyz",
                        "details": [{"field": "name", "reason": "invalid"}],
                    }
                ).encode("utf-8")
                return st, {"Content-Type": "application/json", "Retry-After": "5"}, body

            _MockRouteHandler.routes[("GET", "/api/v1/monitors/42")] = handler
            with self.assertRaises(expected_cls) as ctx:
                client.get_monitor(42)
            err = ctx.exception
            self.assertEqual(err.status_code, status)
            self.assertEqual(err.code, code)
            self.assertEqual(err.request_id, "req_xyz")
            self.assertEqual(err.retry_after, 5)
            self.assertEqual(len(err.details), 1)
            self.assertEqual(err.details[0].field, "name")
            if pred_name != "":
                pred = getattr(err, pred_name)
                self.assertTrue(pred())

    def test_transport_error_raises_sorobeacon_error(self) -> None:
        client = SoroBeaconClient("http://127.0.0.1:1")
        with self.assertRaises(SoroBeaconError):
            client.list_monitors()

    def test_sse_stream_and_ndjson_export(self) -> None:
        alert_json = json.dumps(
            {
                "id": 9007199254740995,
                "monitor_id": 1,
                "rule_id": 2,
                "event_id": "0009007199254740993-0000000001",
                "event_name": "transfer",
                "payload": {"amount": "340282366920938463463374607431768211455"},
                "created_at": "2026-03-01T00:00:00Z",
            }
        )
        _MockRouteHandler.routes[("GET", "/api/v1/alerts/stream")] = lambda _q, _b, _h: (
            200,
            {"Content-Type": "text/event-stream"},
            f"data: {alert_json}\n\n".encode("utf-8"),
        )
        _MockRouteHandler.routes[("GET", "/api/v1/alerts/export")] = lambda _q, _b, _h: (
            200,
            {"Content-Type": "application/x-ndjson"},
            f"{alert_json}\n".encode("utf-8"),
        )
        client = SoroBeaconClient(self.base_url)
        streamed = list(client.stream_alerts(monitor_id=1))
        self.assertEqual(len(streamed), 1)
        self.assertEqual(streamed[0].id, 9007199254740995)

        exported = client.export_alerts_ndjson(monitor_id=1)
        self.assertEqual(len(exported), 1)
        self.assertEqual(exported[0].id, 9007199254740995)


if __name__ == "__main__":
    unittest.main()
