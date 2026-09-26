package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/apiclient"
)

// monitorResponse is a stored monitor as the API returns it.
const monitorResponse = `{"id":5,"name":"My token","contract_ids":["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
	"enabled":true,"created_at":"2026-09-24T10:00:00Z","last_matched_at":null,"channel_ids":[2,3]}`

func TestMonitorListPrintsTableAndJSON(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"monitors":[
			`+monitorResponse+`,
			{"id":6,"name":"Other","contract_ids":["CB8"],"enabled":false,
			 "created_at":"2026-09-24T10:00:00Z","last_matched_at":"2026-09-24T11:00:00Z","channel_ids":[]}
		],"next_cursor":""}`)
	}

	t.Run("table", func(t *testing.T) {
		srv, requests := startInstance(t, handler)
		out, err := runCLIForTest(t, srv, "monitor", "list")
		if err != nil {
			t.Fatalf("monitor list: %v", err)
		}
		for _, want := range []string{"ID", "NAME", "ENABLED", "My token", "Other", "never"} {
			if !strings.Contains(out, want) {
				t.Errorf("table is missing %q:\n%s", want, out)
			}
		}
		if got := (*requests)[0]; got.method != http.MethodGet || got.path != "/api/v1/monitors" {
			t.Errorf("got %s %s, want GET /api/v1/monitors", got.method, got.path)
		}
	})

	t.Run("json", func(t *testing.T) {
		srv, _ := startInstance(t, handler)
		out, err := runCLIForTest(t, srv, "monitor", "list", "--json")
		if err != nil {
			t.Fatalf("monitor list --json: %v", err)
		}
		var monitors []apiclient.Monitor
		if err := json.Unmarshal([]byte(out), &monitors); err != nil {
			t.Fatalf("--json output is not a monitor array: %v\n%s", err, out)
		}
		if len(monitors) != 2 || monitors[0].ID != 5 || monitors[1].Enabled {
			t.Errorf("unexpected monitors: %+v", monitors)
		}
	})

	t.Run("filters", func(t *testing.T) {
		srv, requests := startInstance(t, handler)
		if _, err := runCLIForTest(t, srv, "monitor", "list", "--enabled", "--q", "token", "--limit", "5"); err != nil {
			t.Fatalf("monitor list with filters: %v", err)
		}
		if got := (*requests)[0]; got.method != http.MethodGet {
			t.Errorf("unexpected method %s", got.method)
		}
	})

	t.Run("conflicting filters", func(t *testing.T) {
		srv, _ := startInstance(t, expectNoRequest(t))
		_, err := runCLIForTest(t, srv, "monitor", "list", "--enabled", "--disabled")
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("expected a mutually-exclusive error, got %v", err)
		}
	})

	t.Run("stray argument", func(t *testing.T) {
		srv, _ := startInstance(t, expectNoRequest(t))
		_, err := runCLIForTest(t, srv, "monitor", "list", "5")
		if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Fatalf("expected an unexpected-argument error, got %v", err)
		}
	})
}

func TestMonitorGet(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, monitorResponse)
	})
	out, err := runCLIForTest(t, srv, "monitor", "get", "5")
	if err != nil {
		t.Fatalf("monitor get: %v", err)
	}
	if got := (*requests)[0]; got.method != http.MethodGet || got.path != "/api/v1/monitors/5" {
		t.Errorf("got %s %s, want GET /api/v1/monitors/5", got.method, got.path)
	}
	if !strings.Contains(out, "My token") || !strings.Contains(out, "channels:     2, 3") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestMonitorGetRejectsBadIDs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"not a number", []string{"monitor", "get", "abc"}},
		{"zero", []string{"monitor", "get", "0"}},
		{"missing", []string{"monitor", "get"}},
		{"too many", []string{"monitor", "get", "1", "2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, requests := startInstance(t, expectNoRequest(t))
			_, err := runCLIForTest(t, srv, tt.args...)
			if err == nil {
				t.Fatalf("runCLI %v should fail", tt.args)
			}
			if len(*requests) != 0 {
				t.Errorf("no request should be sent for a bad id, got %d", len(*requests))
			}
		})
	}
}

func TestMonitorCreate(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, monitorResponse)
	})

	out, err := runCLIForTest(t, srv, "monitor", "create",
		"--name", "My token",
		"--contract", "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		"--channel", "2", "--channel", "3",
	)
	if err != nil {
		t.Fatalf("monitor create: %v", err)
	}
	got := (*requests)[0]
	if got.method != http.MethodPost || got.path != "/api/v1/monitors" {
		t.Errorf("got %s %s, want POST /api/v1/monitors", got.method, got.path)
	}
	assertBodyJSON(t, got.body, `{
		"name": "My token",
		"contract_ids": ["CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"],
		"channel_ids": [2, 3]
	}`)
	if !strings.Contains(out, "My token") {
		t.Errorf("created monitor should be printed:\n%s", out)
	}

	// --disabled must send enabled=false rather than leaving it to the
	// server's default of true.
	srv2, requests2 := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, monitorResponse)
	})
	if _, err := runCLIForTest(t, srv2, "monitor", "create", "--name", "n", "--contract", "CA7", "--disabled"); err != nil {
		t.Fatalf("monitor create --disabled: %v", err)
	}
	assertBodyJSON(t, (*requests2)[0].body, `{"name":"n","contract_ids":["CA7"],"enabled":false}`)
}

func TestMonitorCreateValidatesFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing name", []string{"monitor", "create", "--contract", "CA7"}, "--name is required"},
		{"missing contract", []string{"monitor", "create", "--name", "n"}, "--contract is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, requests := startInstance(t, expectNoRequest(t))
			_, err := runCLIForTest(t, srv, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got %v", tt.want, err)
			}
			if len(*requests) != 0 {
				t.Errorf("no request should be sent, got %d", len(*requests))
			}
		})
	}
}

func TestMonitorEnableAndDisable(t *testing.T) {
	tests := []struct {
		command string
		enabled bool
	}{
		{"enable", true},
		{"disable", false},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			// The instance answers with the state the PATCH asked for, which
			// is what the CLI prints back.
			response := strings.Replace(monitorResponse, `"enabled":true`, fmt.Sprintf(`"enabled":%t`, tt.enabled), 1)
			srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, response)
			})
			out, err := runCLIForTest(t, srv, "monitor", tt.command, "5")
			if err != nil {
				t.Fatalf("monitor %s: %v", tt.command, err)
			}
			got := (*requests)[0]
			if got.method != http.MethodPatch || got.path != "/api/v1/monitors/5" {
				t.Errorf("got %s %s, want PATCH /api/v1/monitors/5", got.method, got.path)
			}
			assertBodyJSON(t, got.body, fmt.Sprintf(`{"enabled":%t}`, tt.enabled))
			want := "enabled:      yes"
			if !tt.enabled {
				want = "enabled:      no"
			}
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		})
	}
}

func TestMonitorDelete(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	out, err := runCLIForTest(t, srv, "monitor", "delete", "5")
	if err != nil {
		t.Fatalf("monitor delete: %v", err)
	}
	if got := (*requests)[0]; got.method != http.MethodDelete || got.path != "/api/v1/monitors/5" {
		t.Errorf("got %s %s, want DELETE /api/v1/monitors/5", got.method, got.path)
	}
	if !strings.Contains(out, "deleted monitor 5") {
		t.Errorf("unexpected output: %q", out)
	}

	jsonOut, err := runCLIForTest(t, srv, "monitor", "delete", "5", "--json")
	if err != nil {
		t.Fatalf("monitor delete --json: %v", err)
	}
	var deleted map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &deleted); err != nil {
		t.Fatalf("--json output is not JSON: %v (%s)", err, jsonOut)
	}
	if deleted["deleted"] != true || deleted["id"] != float64(5) {
		t.Errorf("unexpected delete result: %v", deleted)
	}
}

func TestRuleList(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":2,"monitor_id":5,"type":"event_emitted","params":{"event_name":"transfer"},"enabled":true}]`)
	})

	out, err := runCLIForTest(t, srv, "rule", "list", "5")
	if err != nil {
		t.Fatalf("rule list: %v", err)
	}
	if got := (*requests)[0]; got.method != http.MethodGet || got.path != "/api/v1/monitors/5/rules" {
		t.Errorf("got %s %s, want GET /api/v1/monitors/5/rules", got.method, got.path)
	}
	for _, want := range []string{"TYPE", "event_emitted", `{"event_name":"transfer"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	jsonOut, err := runCLIForTest(t, srv, "rule", "list", "5", "--json")
	if err != nil {
		t.Fatalf("rule list --json: %v", err)
	}
	var rules []apiclient.Rule
	if err := json.Unmarshal([]byte(jsonOut), &rules); err != nil {
		t.Fatalf("--json output is not a rule array: %v\n%s", err, jsonOut)
	}
	if len(rules) != 1 || rules[0].Type != "event_emitted" {
		t.Errorf("unexpected rules: %+v", rules)
	}
}

func TestRuleAdd(t *testing.T) {
	t.Run("typed key=value params", func(t *testing.T) {
		srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":2,"monitor_id":5,"type":"frequency_threshold","params":{"count":50,"window":"5m"},"enabled":true}`)
		})

		out, err := runCLIForTest(t, srv, "rule", "add", "5",
			"--type", "frequency_threshold",
			"--param", "count=50",
			"--param", "window=5m",
		)
		if err != nil {
			t.Fatalf("rule add: %v", err)
		}
		got := (*requests)[0]
		if got.method != http.MethodPost || got.path != "/api/v1/monitors/5/rules" {
			t.Errorf("got %s %s, want POST /api/v1/monitors/5/rules", got.method, got.path)
		}
		// count must be a JSON number and window a string: a rule that
		// rejected the params server-side would surface only at runtime.
		assertBodyJSON(t, got.body, `{"type":"frequency_threshold","params":{"count":50,"window":"5m"}}`)
		if !strings.Contains(out, "frequency_threshold") {
			t.Errorf("created rule should be printed:\n%s", out)
		}
	})

	t.Run("raw JSON params for nested shapes", func(t *testing.T) {
		srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":3,"monitor_id":5,"type":"event_emitted","params":{},"enabled":true}`)
		})

		params := `{"event_name":"transfer","topic_equals":{"1":"GDW6SENDER"}}`
		if _, err := runCLIForTest(t, srv, "rule", "add", "5", "--type", "event_emitted", "--params", params); err != nil {
			t.Fatalf("rule add --params: %v", err)
		}
		assertBodyJSON(t, (*requests)[0].body, `{"type":"event_emitted","params":{"event_name":"transfer","topic_equals":{"1":"GDW6SENDER"}}}`)
	})

	t.Run("requires a type", func(t *testing.T) {
		srv, requests := startInstance(t, expectNoRequest(t))
		_, err := runCLIForTest(t, srv, "rule", "add", "5", "--param", "event_name=transfer")
		if err == nil || !strings.Contains(err.Error(), "--type is required") {
			t.Fatalf("expected a --type error, got %v", err)
		}
		if len(*requests) != 0 {
			t.Errorf("no request should be sent, got %d", len(*requests))
		}
	})
}

func TestRuleDelete(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	out, err := runCLIForTest(t, srv, "rule", "delete", "5", "9")
	if err != nil {
		t.Fatalf("rule delete: %v", err)
	}
	if got := (*requests)[0]; got.method != http.MethodDelete || got.path != "/api/v1/monitors/5/rules/9" {
		t.Errorf("got %s %s, want DELETE /api/v1/monitors/5/rules/9", got.method, got.path)
	}
	if !strings.Contains(out, "deleted rule 9") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestChannelListNeverPrintsConfig(t *testing.T) {
	// The fake instance answers with the config included, which a real one
	// never does. If the CLI ever gains a config field, this test fails
	// before the secret can reach a terminal, a log or a CI transcript.
	handler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"channels":[{
			"id":1,"name":"ops-slack","type":"slack","enabled":true,
			"created_at":"2026-09-24T10:00:00Z",
			"config":{"webhook_url":"https://hooks.slack.com/services/T000/TopSecret"}
		}],"next_cursor":""}`)
	}

	for _, args := range [][]string{
		{"channel", "list"},
		{"channel", "list", "--json"},
		{"channel", "list", "--type", "slack"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv, _ := startInstance(t, handler)
			out, err := runCLIForTest(t, srv, args...)
			if err != nil {
				t.Fatalf("runCLI %v: %v", args, err)
			}
			for _, secret := range []string{"TopSecret", "hooks.slack.com", "webhook_url"} {
				if strings.Contains(out, secret) {
					t.Errorf("channel config leaked into output (%q):\n%s", secret, out)
				}
			}
			if !strings.Contains(out, "ops-slack") {
				t.Errorf("output is missing the channel name:\n%s", out)
			}
		})
	}
}

func TestChannelListFilters(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"channels":[],"next_cursor":""}`)
	})

	if _, err := runCLIForTest(t, srv, "channel", "list", "--type", "slack", "--disabled"); err != nil {
		t.Fatalf("channel list: %v", err)
	}
	got := (*requests)[0]
	if got.method != http.MethodGet || got.path != "/api/v1/channels" {
		t.Errorf("got %s %s, want GET /api/v1/channels", got.method, got.path)
	}
}

func TestChannelCreateSendsConfigOnce(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1,"name":"ops-discord","type":"discord","enabled":true,"created_at":"2026-09-24T10:00:00Z"}`)
	})

	out, err := runCLIForTest(t, srv, "channel", "create",
		"--name", "ops-discord",
		"--type", "discord",
		"--config", "webhook_url=https://discord.com/api/webhooks/TopSecret",
	)
	if err != nil {
		t.Fatalf("channel create: %v", err)
	}
	got := (*requests)[0]
	if got.method != http.MethodPost || got.path != "/api/v1/channels" {
		t.Errorf("got %s %s, want POST /api/v1/channels", got.method, got.path)
	}
	if !strings.Contains(got.body, "TopSecret") {
		t.Errorf("config should be sent to the instance, got %s", got.body)
	}
	if strings.Contains(out, "TopSecret") {
		t.Errorf("the CLI must not echo a secret it was given:\n%s", out)
	}
}

func TestChannelTest(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"sent"}`)
	})

	out, err := runCLIForTest(t, srv, "channel", "test", "1")
	if err != nil {
		t.Fatalf("channel test: %v", err)
	}
	if got := (*requests)[0]; got.method != http.MethodPost || got.path != "/api/v1/channels/1/test" {
		t.Errorf("got %s %s, want POST /api/v1/channels/1/test", got.method, got.path)
	}
	if !strings.Contains(out, "test alert sent through channel 1") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestChannelDelete(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	out, err := runCLIForTest(t, srv, "channel", "delete", "1")
	if err != nil {
		t.Fatalf("channel delete: %v", err)
	}
	if got := (*requests)[0]; got.method != http.MethodDelete || got.path != "/api/v1/channels/1" {
		t.Errorf("got %s %s, want DELETE /api/v1/channels/1", got.method, got.path)
	}
	if !strings.Contains(out, "deleted channel 1") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestAPIErrorsSurfaceAndDoNotPanic(t *testing.T) {
	srv, _ := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found","code":"Not Found","request_id":"9f2b"}`)
	})

	_, err := runCLIForTest(t, srv, "monitor", "get", "999")
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	if err.Error() != "not found" {
		t.Errorf("error = %q, want the server's envelope message", err)
	}
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.RequestID != "9f2b" {
		t.Errorf("expected the envelope's request id, got %v", err)
	}
}

func TestTokenComesFromTheEnvironment(t *testing.T) {
	srv, requests := startInstance(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"monitors":[],"next_cursor":""}`)
	})
	t.Setenv("SOROBEACON_URL", srv.URL)
	t.Setenv("SOROBEACON_TOKEN", "env-token")

	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"monitor", "list"}, &out); err != nil {
		t.Fatalf("monitor list: %v", err)
	}
	if got := (*requests)[0].header.Get("Authorization"); got != "Bearer env-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer env-token")
	}
}

// assertBodyJSON compares a request body with an expectation semantically, so
// a test is about the payload rather than key order.
func assertBodyJSON(t *testing.T, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expectation is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("body = %s, want %s", got, want)
	}
}
