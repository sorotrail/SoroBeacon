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

var updateFlag = flag.Bool("update", false, "regenerate clients/python/sorobeacon/_generated.py from internal/api/openapi.json")

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

	if string(committed) != string(generated) {
		t.Fatalf("clients/python/sorobeacon/_generated.py is out of sync with internal/api/openapi.json.\nRun: go test ./clients/python -run TestPythonClientMatchesOpenAPISpec -update")
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
	if string(mutatedOut) == string(committed) {
		t.Fatal("expected spec mutation to produce a different Python client output")
	}

	brokenRefSpec := strings.Replace(string(rawSpec), `"#/components/schemas/Monitor"`, `"#/components/schemas/NonExistentSchema"`, 1)
	if _, err := Generate([]byte(brokenRefSpec)); err == nil {
		t.Fatal("expected broken $ref in OpenAPI spec to fail generation")
	}
}

func TestPythonClientTypechecksAndRunsUsageExample(t *testing.T) {
	pkgDir := PackageDir()

	if mypyPath, err := exec.LookPath("mypy"); err == nil {
		cmd := exec.Command(
			mypyPath,
			"--config-file", filepath.Join(pkgDir, "pyproject.toml"),
			filepath.Join(pkgDir, "sorobeacon"),
			filepath.Join(pkgDir, "examples"),
			filepath.Join(pkgDir, "tests"),
		)
		cmd.Dir = pkgDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mypy --strict failed: %v\n%s", err, string(out))
		}
	} else if os.Getenv("SOROBEACON_REQUIRE_MYPY") == "1" {
		t.Fatalf("mypy not found in PATH while SOROBEACON_REQUIRE_MYPY=1: %v", err)
	} else {
		t.Log("mypy not found in PATH; relying on AST strict annotation checks in test_client.py")
	}

	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("SOROBEACON_REQUIRE_PYTHON") == "1" {
			t.Fatalf("python3 not found in PATH while SOROBEACON_REQUIRE_PYTHON=1: %v", err)
		}
		t.Skip("python3 not found in PATH; skipping runtime execution of Python client tests and example")
	}

	// 1. Run the Python client unit and AST strict type-annotation test suite.
	unitCmd := exec.Command(pythonPath, "-m", "unittest", "discover", "-s", filepath.Join(pkgDir, "tests"), "-v")
	unitCmd.Dir = pkgDir
	if out, err := unitCmd.CombinedOutput(); err != nil {
		t.Fatalf("python3 -m unittest failed: %v\n%s", err, string(out))
	}

	// 2. Run the documented runnable example (examples/usage.py) against a live HTTP test server.
	srv := NewMockSoroBeaconServer(t)
	defer srv.Close()

	exampleCmd := exec.Command(pythonPath, filepath.Join(pkgDir, "examples", "usage.py"))
	exampleCmd.Dir = pkgDir
	exampleCmd.Env = append(os.Environ(),
		"SOROBEACON_URL="+srv.URL,
		"SOROBEACON_TOKEN=sb_test_token_123",
	)
	out, err := exampleCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 examples/usage.py failed: %v\n%s", err, string(out))
	}

	var summary struct {
		Version          string   `json:"version"`
		MonitorNames     []string `json:"monitor_names"`
		TriggeredAlertID int64    `json:"triggered_alert_id"`
		AlertID          int64    `json:"alert_id"`
		WideInt          string   `json:"wide_int"`
		WideDecimal      string   `json:"wide_decimal"`
		AuditID          int64    `json:"audit_id"`
		NotFoundCode     string   `json:"not_found_code"`
	}
	if err := json.Unmarshal(out, &summary); err != nil {
		t.Fatalf("unmarshal usage.py output %q: %v", string(out), err)
	}
	if summary.Version != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0", summary.Version)
	}
	if strings.Join(summary.MonitorNames, ",") != "usdc-transfer-watch,dex-swap-watch" {
		t.Errorf("monitor_names = %v, want [usdc-transfer-watch dex-swap-watch]", summary.MonitorNames)
	}
	if summary.TriggeredAlertID != 9007199254740995 || summary.AlertID != 9007199254740995 {
		t.Errorf("unexpected alert IDs: triggered=%d alert=%d", summary.TriggeredAlertID, summary.AlertID)
	}
	if summary.WideInt != "340282366920938463463374607431768211455" {
		t.Errorf("wide_int = %q", summary.WideInt)
	}
	if summary.WideDecimal != "34028236692093846346337460743176.8211455" {
		t.Errorf("wide_decimal = %q", summary.WideDecimal)
	}
	if summary.AuditID != 9007199254740999 {
		t.Errorf("audit_id = %d, want 9007199254740999", summary.AuditID)
	}
	if summary.NotFoundCode != "NOT_FOUND" {
		t.Errorf("not_found_code = %q, want NOT_FOUND", summary.NotFoundCode)
	}
}

// NewMockSoroBeaconServer starts a mock SoroBeacon HTTP API server for end-to-end
// client verification.
func NewMockSoroBeaconServer(t *testing.T) *httptest.Server {
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
