package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/apiclient"
)

// cliRequest is one request the CLI made against a fake instance.
type cliRequest struct {
	method string
	path   string
	body   string
	header http.Header
}

// startInstance runs a fake SoroBeacon instance. handler answers the
// requests; the requests themselves are recorded so a test can assert on the
// method, path and body the CLI produced.
func startInstance(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *[]cliRequest) {
	t.Helper()
	requests := &[]cliRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*requests = append(*requests, cliRequest{
			method: r.Method,
			path:   r.URL.Path,
			body:   string(body),
			header: r.Header.Clone(),
		})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, requests
}

// expectNoRequest is the handler for tests where validation should stop the
// command before it touches the network.
func expectNoRequest(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}
}

// runCLIForTest drives the CLI exactly as main does, pointed at a test
// instance. The environment is cleared first so a developer's own
// SOROBEACON_URL or SOROBEACON_TOKEN cannot change the result.
func runCLIForTest(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	t.Setenv("SOROBEACON_URL", "")
	t.Setenv("SOROBEACON_TOKEN", "")
	var out bytes.Buffer
	err := runCLI(context.Background(), append(args, "--url", srv.URL), &out)
	return out.String(), err
}

func TestSplitGlobalFlags(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		rest  []string
		url   string
		token string
		json  bool
		help  bool
		want  error
	}{
		{
			name: "no globals",
			args: []string{"monitor", "list"},
			rest: []string{"monitor", "list"},
		},
		{
			name: "url before the command",
			args: []string{"--url", "http://beacon.example", "monitor", "list"},
			rest: []string{"monitor", "list"},
			url:  "http://beacon.example",
		},
		{
			name: "url after the command",
			args: []string{"monitor", "list", "--url", "http://beacon.example"},
			rest: []string{"monitor", "list"},
			url:  "http://beacon.example",
		},
		{
			name:  "single dash and equals forms",
			args:  []string{"channel", "list", "-url=http://beacon.example", "--token=t0k"},
			rest:  []string{"channel", "list"},
			url:   "http://beacon.example",
			token: "t0k",
		},
		{
			name: "json flag",
			args: []string{"--json", "monitor", "list"},
			rest: []string{"monitor", "list"},
			json: true,
		},
		{
			name: "json with an explicit value",
			args: []string{"monitor", "list", "--json=false"},
			rest: []string{"monitor", "list"},
		},
		{
			name: "top level help",
			args: []string{"--help"},
			help: true,
		},
		{
			name: "command flags pass through untouched",
			args: []string{"rule", "add", "1", "--type", "frequency_threshold", "--param", "window=5m"},
			rest: []string{"rule", "add", "1", "--type", "frequency_threshold", "--param", "window=5m"},
		},
		{
			name: "help after the command belongs to the command",
			args: []string{"monitor", "list", "-h"},
			rest: []string{"monitor", "list", "-h"},
		},
		{
			name: "missing url value",
			args: []string{"monitor", "list", "--url"},
			want: errInvalidTestArgs,
		},
		{
			name: "json value is not a boolean",
			args: []string{"--json=maybe", "monitor", "list"},
			want: errInvalidTestArgs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globals, rest, err := splitGlobalFlags(tt.args)
			if tt.want != nil {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("splitGlobalFlags: %v", err)
			}
			if !reflect.DeepEqual(rest, tt.rest) {
				t.Errorf("rest = %v, want %v", rest, tt.rest)
			}
			if globals.url != tt.url || globals.token != tt.token {
				t.Errorf("url/token = %q/%q, want %q/%q", globals.url, globals.token, tt.url, tt.token)
			}
			if globals.json != tt.json || globals.help != tt.help {
				t.Errorf("json/help = %v/%v, want %v/%v", globals.json, globals.help, tt.json, tt.help)
			}
		})
	}
}

// errInvalidTestArgs marks table rows that expect a parse failure.
var errInvalidTestArgs = errors.New("expected a parse error")

func TestPermuteArgsPutsFlagsFirst(t *testing.T) {
	fs := newFlagSet("test")
	fs.String("name", "", "a flag that takes a value")
	fs.Bool("disabled", false, "a flag that takes none")

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "flags before the positional argument are untouched",
			args: []string{"--name", "x", "5"},
			want: []string{"--name", "x", "5"},
		},
		{
			name: "a positional argument first still gets its flags parsed",
			args: []string{"5", "--name", "x"},
			want: []string{"--name", "x", "5"},
		},
		{
			name: "a boolean flag consumes no value",
			args: []string{"5", "--disabled", "--name", "x"},
			want: []string{"--disabled", "--name", "x", "5"},
		},
		{
			name: "the equals form carries its own value",
			args: []string{"5", "--name=x"},
			want: []string{"--name=x", "5"},
		},
		{
			name: "unknown flags are left for the flag set to reject",
			args: []string{"5", "--bogus", "x"},
			want: []string{"--bogus", "5", "x"},
		},
		{
			name: "a lone dash is positional",
			args: []string{"-"},
			want: []string{"-"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := permuteArgs(fs, tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("permuteArgs(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"help"}, &out); err != nil {
		t.Fatalf("runCLI help: %v", err)
	}
	usage := out.String()
	for _, want := range []string{
		"monitor list|get|create|delete|enable|disable",
		"rule    list|add|delete",
		"channel list|create|delete|test",
		"SOROBEACON_URL",
		apiclient.DefaultBaseURL,
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("help output is missing %q:\n%s", want, usage)
		}
	}
}

func TestHelpVariantsPrintUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no arguments", nil, "sorobeacon — monitor and manage"},
		{"help command", []string{"help"}, "sorobeacon — monitor and manage"},
		{"global flag", []string{"--help"}, "sorobeacon — monitor and manage"},
		{"monitor group", []string{"monitor"}, "sorobeacon monitor — list and manage monitors"},
		{"monitor group flag", []string{"monitor", "-h"}, "sorobeacon monitor — list and manage monitors"},
		{"rule group", []string{"rule", "--help"}, "sorobeacon rule — list and manage a monitor's rules"},
		{"channel group", []string{"channel", "help"}, "sorobeacon channel — list and manage notification channels"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			// A command that only printed its usage must not be an error:
			// asking for help is a successful invocation.
			if err := runCLI(context.Background(), tt.args, &out); err != nil {
				t.Fatalf("runCLI %v: %v", tt.args, err)
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("output is missing %q:\n%s", tt.want, out.String())
			}
		})
	}
}

func TestUnknownCommandsAreErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"unknown monitor command", []string{"monitor", "frobnicate"}, "unknown monitor command"},
		{"unknown rule command", []string{"rule", "frobnicate"}, "unknown rule command"},
		{"unknown channel command", []string{"channel", "frobnicate"}, "unknown channel command"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runCLI(context.Background(), tt.args, &out)
			if err == nil {
				t.Fatalf("runCLI %v should fail", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestBuildJSONObject(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		kv    []string
		raw   string
		want  string
		fails bool
	}{
		{
			name: "empty becomes an object",
			flag: "--param",
			want: `{}`,
		},
		{
			name: "values are typed by their JSON spelling",
			flag: "--param",
			kv:   []string{"count=50", "window=5m", "nested={\"a\":1}", "flag=true"},
			want: `{"count":50,"window":"5m","nested":{"a":1},"flag":true}`,
		},
		{
			name: "quoted values stay strings",
			flag: "--param",
			kv:   []string{`threshold="10000000000000000000"`},
			want: `{"threshold":"10000000000000000000"}`,
		},
		{
			name: "raw JSON object",
			flag: "--param",
			raw:  `{"event_name":"transfer","topic_equals":{"1":"GDW6"}}`,
			want: `{"event_name":"transfer","topic_equals":{"1":"GDW6"}}`,
		},
		{
			name:  "raw and key=value together",
			flag:  "--param",
			kv:    []string{"count=50"},
			raw:   `{}`,
			fails: true,
		},
		{
			name:  "raw must be an object",
			flag:  "--param",
			raw:   `[1,2]`,
			fails: true,
		},
		{
			name:  "raw null is not an object",
			flag:  "--param",
			raw:   `null`,
			fails: true,
		},
		{
			name:  "missing equals",
			flag:  "--param",
			kv:    []string{"count"},
			fails: true,
		},
		{
			name:  "empty key",
			flag:  "--param",
			kv:    []string{"=5"},
			fails: true,
		},
		{
			name:  "duplicate key",
			flag:  "--param",
			kv:    []string{"count=50", "count=60"},
			fails: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildJSONObject(tt.flag, tt.kv, tt.raw)
			if tt.fails {
				if err == nil {
					t.Fatalf("expected an error, got %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildJSONObject: %v", err)
			}
			var gotValue, wantValue any
			if err := json.Unmarshal(got, &gotValue); err != nil {
				t.Fatalf("result is not JSON: %v (%s)", err, got)
			}
			if err := json.Unmarshal([]byte(tt.want), &wantValue); err != nil {
				t.Fatalf("expectation is not JSON: %v", err)
			}
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestConfigFlagNamesItsJSONSibling(t *testing.T) {
	_, err := buildJSONObject("--config", []string{"port=587"}, `{}`)
	if err == nil {
		t.Fatal("expected an error when both --config and --config-json are given")
	}
	if !strings.Contains(err.Error(), "--config-json") {
		t.Errorf("error should name --config-json, got %q", err)
	}
}

func TestReportCLIError(t *testing.T) {
	apiErr := &apiclient.APIError{
		Status:    http.StatusBadRequest,
		Message:   "validation failed",
		RequestID: "9f2b",
		Details: []apiclient.FieldError{
			{Field: "params.count", Reason: "must be a positive integer"},
			{Field: "type", Reason: "unknown rule type"},
		},
	}

	var out bytes.Buffer
	reportCLIError(&out, fmt.Errorf("wrapped: %w", apiErr))
	got := out.String()
	for _, want := range []string{
		"sorobeacon: wrapped: validation failed",
		"sorobeacon:   params.count: must be a positive integer",
		"sorobeacon:   type: unknown rule type",
		"request id 9f2b",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("error output is missing %q:\n%s", want, got)
		}
	}

	// An ordinary failure with no envelope still prints one line and no
	// stack trace.
	out.Reset()
	reportCLIError(&out, errors.New("connection refused"))
	if got := out.String(); got != "sorobeacon: connection refused\n" {
		t.Errorf("unexpected output: %q", got)
	}
}
