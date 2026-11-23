package typescript

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

var updateFlag = flag.Bool("update", false, "regenerate clients/typescript/src/index.ts from internal/api/openapi.json")

func TestTypeScriptClientMatchesOpenAPISpec(t *testing.T) {
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
		t.Fatalf("read committed TypeScript client %s: %v (run: go test ./clients/typescript -run TestTypeScriptClientMatchesOpenAPISpec -update)", outPath, err)
	}

	if string(committed) != string(generated) {
		t.Fatalf("clients/typescript/src/index.ts is out of sync with internal/api/openapi.json.\nRun: go test ./clients/typescript -run TestTypeScriptClientMatchesOpenAPISpec -update")
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
		t.Fatal("expected spec mutation to produce a different TypeScript client output")
	}

	brokenRefSpec := strings.Replace(string(rawSpec), `"#/components/schemas/Monitor"`, `"#/components/schemas/NonExistentSchema"`, 1)
	if _, err := Generate([]byte(brokenRefSpec)); err == nil {
		t.Fatal("expected broken $ref in OpenAPI spec to fail generation")
	}
}

func TestTypeScriptClientTypechecksAndRunsUsageExample(t *testing.T) {
	pkgDir := PackageDir()

	if tscPath, err := exec.LookPath("tsc"); err == nil {
		cmd := exec.Command(tscPath, "--noEmit", "--project", filepath.Join(pkgDir, "tsconfig.json"))
		cmd.Dir = pkgDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("tsc --noEmit failed: %v\n%s", err, string(out))
		}
	} else if os.Getenv("SOROBEACON_REQUIRE_TSC") == "1" {
		t.Fatalf("tsc not found in PATH while SOROBEACON_REQUIRE_TSC=1: %v", err)
	} else {
		t.Log("tsc not found in PATH; skipping tsc --noEmit check")
	}

	nodePath, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("SOROBEACON_REQUIRE_NODE") == "1" {
			t.Fatalf("node not found in PATH while SOROBEACON_REQUIRE_NODE=1: %v", err)
		}
		t.Skip("node not found in PATH; skipping runtime execution of examples/usage.ts")
	}

	srv := newMockSoroBeaconServer(t)
	defer srv.Close()

	// 1. Run the documented runnable example (examples/usage.ts) against the mock HTTP server.
	exampleCmd := exec.Command(nodePath, filepath.Join(pkgDir, "examples", "usage.ts"))
	exampleCmd.Dir = pkgDir
	exampleCmd.Env = append(os.Environ(),
		"SOROBEACON_URL="+srv.URL,
		"SOROBEACON_TOKEN=sb_test_token_123",
	)
	if out, err := exampleCmd.CombinedOutput(); err != nil {
		t.Fatalf("node examples/usage.ts failed: %v\n%s", err, string(out))
	}

	// 2. Run behavioral verification of wide-int JSON round-trip, multi-page pagination,
	// token secret redaction, and error taxonomy helpers.
	verifyScript := filepath.Join(t.TempDir(), "verify_client.ts")
	scriptContent := `import {
  SoroBeaconClient,
  SoroBeaconAPIError,
  parseJSONPreservingWideInts,
  stringifyJSONWithWideInts,
  toBigInt,
} from "` + filepath.ToSlash(filepath.Join(pkgDir, "src", "index.ts")) + `";

async function run(): Promise<void> {
  // Verify wide integer JSON round-trip without precision loss.
  const raw = '{"id":9007199254740993,"neg":-9007199254740995,"safe":42,"text":"id: 9007199254740999","arr":[9007199254740997,10]}';
  const parsed = parseJSONPreservingWideInts<{
    id: number | ` + "`${number}`" + `;
    neg: number | ` + "`${number}`" + `;
    safe: number;
    text: string;
    arr: Array<number | ` + "`${number}`" + `>;
  }>(raw);

  if (parsed.id !== "9007199254740993" || toBigInt(parsed.id) !== 9007199254740993n) {
    throw new Error("Expected parsed.id to preserve 9007199254740993, got " + String(parsed.id));
  }
  if (parsed.neg !== "-9007199254740995" || toBigInt(parsed.neg) !== -9007199254740995n) {
    throw new Error("Expected parsed.neg to preserve -9007199254740995, got " + String(parsed.neg));
  }
  if (parsed.safe !== 42) {
    throw new Error("Expected parsed.safe to be 42");
  }
  if (parsed.text !== "id: 9007199254740999") {
    throw new Error("Expected string literal digits to remain untouched");
  }
  if (parsed.arr[0] !== "9007199254740997" || parsed.arr[1] !== 10) {
    throw new Error("Expected array wide int preservation");
  }

  const reserialized = stringifyJSONWithWideInts({
    id: 9007199254740993n,
    neg: -9007199254740995n,
  });
  if (!reserialized.includes('"id":9007199254740993') || !reserialized.includes('"neg":-9007199254740995')) {
    throw new Error("Expected bigint fields to serialize as unquoted JSON numbers: " + reserialized);
  }

  const client = new SoroBeaconClient({
    baseUrl: process.env.SOROBEACON_URL!,
    token: "sb_test_token_123",
  });

  // Verify wide 64-bit integer path parameter and request body serialization.
  await client.triggerEvaluation({
    monitor_id: 9007199254740993n,
    source: "soroban-rpc",
  });

  // Verify multi-page cursor pagination across pages.
  const monitorNames: string[] = [];
  for await (const m of client.paginateMonitors({ limit: 1 })) {
    monitorNames.push(m.name);
  }
  if (monitorNames.join(",") !== "usdc-transfer-watch,dex-swap-watch") {
    throw new Error("Expected multi-page monitors, got " + monitorNames.join(","));
  }

  // Verify token creation returns secret once and listTokens omits it.
  const createdTok = await client.createToken({ name: "ci-runner", scopes: ["monitors:read"] });
  if (createdTok.token !== "sb_live_once_secret") {
    throw new Error("Expected one-time token secret on createToken");
  }
  const listedToks = await client.listTokens();
  if (listedToks.tokens.length !== 1 || ("token" in listedToks.tokens[0] && listedToks.tokens[0].token !== undefined)) {
    throw new Error("Expected token secret to be omitted on listTokens");
  }

  // Verify audit log wide int IDs via paginateAuditLogs.
  const auditIds: bigint[] = [];
  for await (const entry of client.paginateAuditLogs({ limit: 10 })) {
    auditIds.push(toBigInt(entry.id));
  }
  if (auditIds[0] !== 9007199254740999n) {
    throw new Error("Expected wide int64 audit id 9007199254740999n, got " + String(auditIds[0]));
  }
}

run().catch((err) => {
  console.error(err);
  process.exit(1);
});
`
	if err := os.WriteFile(verifyScript, []byte(scriptContent), 0o644); err != nil {
		t.Fatalf("write verify script: %v", err)
	}
	verifyCmd := exec.Command(nodePath, verifyScript)
	verifyCmd.Dir = pkgDir
	verifyCmd.Env = append(os.Environ(), "SOROBEACON_URL="+srv.URL)
	if out, err := verifyCmd.CombinedOutput(); err != nil {
		t.Fatalf("node verify_client.ts failed: %v\n%s", err, string(out))
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
