package python

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

var updateFlag = flag.Bool("update", false, "regenerate clients/python/sorobeacon/__init__.py from internal/api/openapi.json")

func TestPythonClientMatchesOpenAPISpec(t *testing.T) {
	specPath := DefaultSpecPath()
	outPath := DefaultOutputPath()

	generated, err := GenerateFromFile(specPath)
	if err != nil {
		t.Fatalf("GenerateFromFile(%s): %v", specPath, err)
	}

	if *updateFlag {
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(outPath), err)
		}
		if err := os.WriteFile(outPath, generated, 0o644); err != nil {
			t.Fatalf("write %s: %v", outPath, err)
		}
	}

	committed, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read committed Python client %s: %v (run: go test ./clients/python -run TestPythonClientMatchesOpenAPISpec -update)", outPath, err)
	}

	normalizedCommitted := strings.ReplaceAll(string(committed), "\r\n", "\n")
	normalizedGenerated := strings.ReplaceAll(string(generated), "\r\n", "\n")
	if normalizedCommitted != normalizedGenerated {
		t.Fatalf("clients/python/sorobeacon/__init__.py is out of sync with internal/api/openapi.json.\nRun: go test ./clients/python -run TestPythonClientMatchesOpenAPISpec -update")
	}

	// Mutation check: verify that changing internal/api/openapi.json changes the generated output
	// and that dangling $ref schemas are rejected.
	rawSpec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	mutatedSpec := strings.Replace(string(rawSpec), `"summary": "List monitors"`, `"summary": "List all monitors (mutated)"`, 1)
	if mutatedSpec == string(rawSpec) {
		t.Fatal("expected spec mutation to modify raw spec content")
	}
	mutatedOut, err := Generate([]byte(mutatedSpec))
	if err != nil {
		t.Fatalf("Generate on mutated spec: %v", err)
	}
	if strings.ReplaceAll(string(mutatedOut), "\r\n", "\n") == normalizedCommitted {
		t.Fatal("expected spec mutation to produce a different Python client output")
	}

	brokenRefSpec := strings.Replace(string(rawSpec), `"#/components/schemas/Monitor"`, `"#/components/schemas/NonExistentSchema"`, 1)
	if _, err := Generate([]byte(brokenRefSpec)); err == nil {
		t.Fatal("expected broken $ref in OpenAPI spec to fail generation")
	}
}

func findPythonBinary() (string, error) {
	for _, candidate := range []string{"python3", "python"} {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		// On Windows, the Microsoft Store stub for python3 exits non-zero on --version.
		cmd := exec.Command(path, "-c", "import sys; assert sys.version_info >= (3, 10)")
		if err := cmd.Run(); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no working python3/python >= 3.10 found in PATH")
}

func TestPythonClientTypechecksAndRunsUsageExample(t *testing.T) {
	pkgDir := PackageDir()

	pythonPath, err := findPythonBinary()
	if err != nil {
		if os.Getenv("SOROBEACON_REQUIRE_PYTHON") == "1" {
			t.Fatalf("python not found in PATH while SOROBEACON_REQUIRE_PYTHON=1: %v", err)
		}
		t.Skipf("python not found in PATH; skipping runtime execution of examples/usage.py: %v", err)
	}

	// 1. Bytecode compilation check across the generated module and runnable example.
	compileCmd := exec.Command(
		pythonPath,
		"-m",
		"py_compile",
		filepath.Join(pkgDir, "sorobeacon", "__init__.py"),
		filepath.Join(pkgDir, "examples", "usage.py"),
	)
	compileCmd.Dir = pkgDir
	if out, err := compileCmd.CombinedOutput(); err != nil {
		t.Fatalf("python -m py_compile failed: %v\n%s", err, string(out))
	}

	// 2. If mypy is installed on PATH, run strict static type-checking.
	if mypyPath, err := exec.LookPath("mypy"); err == nil {
		mypyCmd := exec.Command(mypyPath, "--config-file", filepath.Join(pkgDir, "pyproject.toml"))
		mypyCmd.Dir = pkgDir
		if out, err := mypyCmd.CombinedOutput(); err != nil {
			t.Fatalf("mypy failed: %v\n%s", err, string(out))
		}
	} else if os.Getenv("SOROBEACON_REQUIRE_MYPY") == "1" {
		t.Fatalf("mypy not found in PATH while SOROBEACON_REQUIRE_MYPY=1: %v", err)
	} else {
		t.Log("mypy not found in PATH; relying on runtime get_type_hints verification")
	}

	srv := newMockSoroBeaconServer(t)
	defer srv.Close()

	baseEnv := append(os.Environ(),
		"PYTHONPATH="+pkgDir,
		"SOROBEACON_URL="+srv.URL,
		"SOROBEACON_TOKEN=sb_test_token_123",
	)

	// 3. Run the documented runnable example (examples/usage.py) against the mock HTTP server.
	exampleCmd := exec.Command(pythonPath, filepath.Join(pkgDir, "examples", "usage.py"))
	exampleCmd.Dir = pkgDir
	exampleCmd.Env = baseEnv
	if out, err := exampleCmd.CombinedOutput(); err != nil {
		t.Fatalf("python examples/usage.py failed: %v\n%s", err, string(out))
	}

	// 4. Run behavioral and type-annotation verification covering:
	//    - typing.get_type_hints() resolution across all generated TypedDicts and methods
	//    - wide integer JSON round-trip (Decimal and str, never float)
	//    - channel.config and token.token redaction on read models
	//    - multi-page iterator pagination across monitors, alerts, and audit logs
	//    - typed SoroBeaconAPIError taxonomy predicates
	verifyScript := filepath.Join(t.TempDir(), "verify_client.py")
	scriptContent := `from __future__ import annotations

import inspect
import os
from decimal import Decimal
from typing import get_type_hints

import sorobeacon
from sorobeacon import (
    Channel,
    ChannelInput,
    APIToken,
    CreatedAPIToken,
    SoroBeaconAPIError,
    SoroBeaconClient,
    parse_json_preserving_wide_ints,
    stringify_json_with_wide_ints,
    to_decimal,
    to_int,
)


def verify_type_annotations() -> None:
    # Every TypedDict and SoroBeaconClient method annotation must resolve cleanly.
    for name, obj in inspect.getmembers(sorobeacon):
        if isinstance(obj, type) and issubclass(obj, dict) and hasattr(obj, "__annotations__"):
            hints = get_type_hints(obj, include_extras=True)
            if not hints and name not in ("JsonObject",):
                raise AssertionError(f"Expected type hints on {name}")
            # Verify no schema field is typed as float.
            for field_name, field_type in hints.items():
                if field_type is float or "float" in str(field_type):
                    raise AssertionError(
                        f"Schema {name}.{field_name} must not use float: {field_type}"
                    )

    for method_name, method in inspect.getmembers(SoroBeaconClient, predicate=inspect.isfunction):
        if method_name.startswith("_") and method_name != "__init__":
            continue
        get_type_hints(method)

    # Channel read model must not promise "config"; ChannelInput must require it.
    channel_hints = get_type_hints(Channel, include_extras=True)
    if "config" in channel_hints:
        raise AssertionError("Channel read TypedDict must not declare redacted 'config' field")
    channel_input_hints = get_type_hints(ChannelInput, include_extras=True)
    if "config" not in channel_input_hints:
        raise AssertionError("ChannelInput TypedDict must declare 'config' field")

    # APIToken read model must not promise "token"; CreatedAPIToken must include it.
    token_hints = get_type_hints(APIToken, include_extras=True)
    if "token" in token_hints:
        raise AssertionError("APIToken read TypedDict must not declare redacted 'token' field")
    created_token_hints = get_type_hints(CreatedAPIToken, include_extras=True)
    if "token" not in created_token_hints:
        raise AssertionError("CreatedAPIToken TypedDict must declare 'token' field")


def verify_runtime_behavior() -> None:
    raw = (
        '{"id":9007199254740993,"neg":-9007199254740995,"safe":42,'
        '"rate":99.95,"text":"id: 9007199254740999","arr":[9007199254740997,10]}'
    )
    parsed = parse_json_preserving_wide_ints(raw)

    if parsed["id"] != "9007199254740993":
        raise AssertionError(f"Expected wide int string '9007199254740993', got {parsed['id']!r}")
    if to_int(parsed["id"]) != 9007199254740993 or to_decimal(parsed["id"]) != Decimal("9007199254740993"):
        raise AssertionError("Wide int conversion mismatch")
    if parsed["neg"] != "-9007199254740995" or to_int(parsed["neg"]) != -9007199254740995:
        raise AssertionError(f"Expected wide negative int preservation, got {parsed['neg']!r}")
    if parsed["safe"] != 42 or isinstance(parsed["safe"], float):
        raise AssertionError(f"Expected safe int 42, got {parsed['safe']!r}")
    if not isinstance(parsed["rate"], Decimal) or isinstance(parsed["rate"], float):
        raise AssertionError(f"Expected fractional JSON number to parse as Decimal, got {type(parsed['rate'])}")
    if parsed["text"] != "id: 9007199254740999":
        raise AssertionError("Expected string literal digits to remain untouched")
    if parsed["arr"][0] != "9007199254740997" or parsed["arr"][1] != 10:
        raise AssertionError("Expected array wide int preservation")

    # Verify float is strictly rejected by to_decimal, to_int, and stringify_json_with_wide_ints.
    for fn in (to_decimal, to_int, stringify_json_with_wide_ints):
        try:
            fn(123.45)  # type: ignore[arg-type]
            raise AssertionError(f"Expected {fn.__name__} to reject float")
        except TypeError:
            pass

    reserialized = stringify_json_with_wide_ints(
        {
            "id": 9007199254740993,
            "dec_id": Decimal("9007199254740995"),
            "neg": Decimal("-9007199254740995"),
        }
    )
    if (
        '"id":9007199254740993' not in reserialized
        or '"dec_id":9007199254740995' not in reserialized
        or '"neg":-9007199254740995' not in reserialized
    ):
        raise AssertionError(f"Expected wide ints/Decimals to serialize as exact JSON numbers: {reserialized}")

    client = SoroBeaconClient(
        base_url=os.environ["SOROBEACON_URL"],
        token="sb_test_token_123",
    )

    # Verify wide 64-bit integer request body serialization via Decimal.
    trig = client.trigger_evaluation(
        {
            "monitor_id": Decimal("9007199254740993"),
            "source": "soroban-rpc",
        }
    )
    if to_int(trig["alert_id"]) != 9007199254740995:
        raise AssertionError(f"Unexpected trigger alert_id: {trig}")

    # Verify multi-page cursor pagination across pages.
    monitor_names = [m["name"] for m in client.paginate_monitors({"limit": 1})]
    if monitor_names != ["usdc-transfer-watch", "dex-swap-watch"]:
        raise AssertionError(f"Expected multi-page monitors, got {monitor_names}")

    # Verify token creation returns secret once and list_tokens omits it.
    created_tok = client.create_token({"name": "ci-runner", "scopes": ["monitors:read"]})
    if created_tok.get("token") != "sb_live_once_secret":
        raise AssertionError("Expected one-time token secret on create_token")
    listed_toks = client.list_tokens()
    if len(listed_toks["tokens"]) != 1 or "token" in listed_toks["tokens"][0]:
        raise AssertionError("Expected token secret to be omitted on list_tokens")

    # Verify audit log wide int IDs via paginate_audit_logs.
    audit_ids = [to_int(entry["id"]) for entry in client.paginate_audit_logs({"limit": 10})]
    if audit_ids != [9007199254740999]:
        raise AssertionError(f"Expected wide int64 audit id 9007199254740999, got {audit_ids}")


if __name__ == "__main__":
    verify_type_annotations()
    verify_runtime_behavior()
`
	if err := os.WriteFile(verifyScript, []byte(scriptContent), 0o644); err != nil {
		t.Fatalf("write verify script: %v", err)
	}
	verifyCmd := exec.Command(pythonPath, verifyScript)
	verifyCmd.Dir = pkgDir
	verifyCmd.Env = baseEnv
	if out, err := verifyCmd.CombinedOutput(); err != nil {
		t.Fatalf("python verify_client.py failed: %v\n%s", err, string(out))
	}
}

func newMockSoroBeaconServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var monitorSeq atomic.Int64

	mux.HandleFunc("/api/v1/channels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			cfg, _ := payload["config"].(map[string]any)
			if cfg == nil || cfg["url"] == nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"channel config.url is required","code":"INVALID_ARGUMENT"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			// Channel response intentionally omits "config" to match store.Channel secret redaction.
			_, _ = io.WriteString(w, `{
				"id":1,
				"name":"ops-webhook",
				"type":"webhook",
				"enabled":true,
				"created_at":"2026-03-01T00:00:00Z"
			}`)
			return
		}
		_, _ = io.WriteString(w, `{"channels":[],"next_cursor":""}`)
	})

	mux.HandleFunc("/api/v1/channels/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/api/v1/monitors", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			id := monitorSeq.Add(1) + 100
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			name, _ := payload["name"].(string)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{
				"id":%d,
				"name":%q,
				"contract_ids":["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"],
				"enabled":true,
				"created_at":"2026-03-01T00:00:00Z"
			}`, id, name)
		case http.MethodGet:
			cursor := r.URL.Query().Get("cursor")
			if cursor == "" {
				_, _ = io.WriteString(w, `{
					"monitors":[{
						"id":101,
						"name":"usdc-transfer-watch",
						"contract_ids":["CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"],
						"enabled":true,
						"created_at":"2026-03-01T00:00:00Z"
					}],
					"next_cursor":"cursor_page_2"
				}`)
				return
			}
			_, _ = io.WriteString(w, `{
				"monitors":[{
					"id":102,
					"name":"dex-swap-watch",
					"contract_ids":["CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"],
					"enabled":true,
					"created_at":"2026-03-01T00:01:00Z"
				}],
				"next_cursor":""
			}`)
		}
	})

	mux.HandleFunc("/api/v1/monitors/999999", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "req_404")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"monitor 999999 not found","code":"NOT_FOUND","request_id":"req_404"}`)
	})

	mux.HandleFunc("/api/v1/monitors/101/rules", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{
			"id":201,
			"monitor_id":101,
			"type":"event_emitted",
			"params":{"event_name":"transfer"},
			"enabled":true
		}`)
	})

	mux.HandleFunc("/api/v1/monitors/101/rules/201", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/api/v1/monitors/101", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/api/v1/monitors/102", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/api/v1/alerts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"alerts":[{
				"id":9007199254740995,
				"monitor_id":101,
				"rule_id":201,
				"event_id":"0009007199254740993-0000000001",
				"event_name":"transfer",
				"ledger":9007199254740993,
				"payload":{
					"contract_id":"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
					"event_name":"transfer",
					"ledger":9007199254740993,
					"topics":["transfer","GABC..."],
					"data":{"amount":"340282366920938463463374607431768211455","asset":"USDC"}
				},
				"created_at":"2026-03-01T00:02:00Z"
			}],
			"next_cursor":""
		}`)
	})

	mux.HandleFunc("/api/v1/triggers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"monitor_id":9007199254740993`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"expected exact wide int64 monitor_id 9007199254740993","code":"INVALID_ARGUMENT"}`)
			return
		}
		_, _ = io.WriteString(w, `{"alert_id":9007199254740995,"note":"evaluated"}`)
	})

	mux.HandleFunc("/api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":"1.0.0","commit":"abc1234","build_date":"2026-03-01T00:00:00Z"}`)
	})

	mux.HandleFunc("/api/v1/tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"id":1,
				"name":"ci-runner",
				"prefix":"sb_live_",
				"scopes":["monitors:read"],
				"created_at":"2026-03-01T00:00:00Z",
				"token":"sb_live_once_secret"
			}`)
			return
		}
		_, _ = io.WriteString(w, `{"tokens":[{
			"id":1,
			"name":"ci-runner",
			"prefix":"sb_live_",
			"scopes":["monitors:read"],
			"created_at":"2026-03-01T00:00:00Z"
		}]}`)
	})

	mux.HandleFunc("/api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"entries":[{
				"id":9007199254740999,
				"actor":"token:1",
				"action":"monitor.create",
				"target_type":"monitor",
				"target_id":101,
				"target_name":"usdc-transfer-watch",
				"created_at":"2026-03-01T00:00:00Z"
			}],
			"next_cursor":0
		}`)
	})

	return httptest.NewServer(mux)
}
