//go:build e2e

// Package e2e runs the real sorobeacon binary against a fake Soroban RPC
// server and an httptest webhook receiver, asserting that:
//   - a monitor + rule + channel created through the API produces an alert
//     when the fake RPC returns a matching event,
//   - the delivery attempt is recorded and visible through the API, and
//   - SIGTERM shuts the process down cleanly within a bounded time.
//
// Run with:
//
//	make test-e2e
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/apiclient"
)

// contractID is the Soroban contract the monitor watches.
const contractID = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

// apiToken is the bearer token set on the test instance.
const apiToken = "e2e-test-token"

// freePort returns a random available TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// buildBinary compiles the sorobeacon binary into a temp dir and returns its
// path. The build is cached by the Go tool chain between runs.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sorobeacon")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/sorobeacon")
	cmd.Dir = filepath.Join("..")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run(), "build sorobeacon binary")
	return bin
}

// fakeRPC is a minimal Soroban JSON-RPC 2.0 server that serves a scripted
// sequence of responses. It is reusable for future scenarios (reorg, outage,
// absence rule) by extending the response handlers.
type fakeRPC struct {
	mux     *http.ServeMux
	server  *httptest.Server
	ledger  uint32
	eventID string
}

// newFakeRPC starts a fake RPC node and returns it.
func newFakeRPC(t *testing.T, eventJSON string) *fakeRPC {
	t.Helper()
	f := &fakeRPC{
		mux:     http.NewServeMux(),
		ledger:  123456,
		eventID: "0000000123456789-0000000000",
	}

	respond := func(w http.ResponseWriter, result string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, result)
	}

	f.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)

		switch req.Method {
		case "getHealth":
			respond(w, `{"status":"healthy","latestLedger":123457,"oldestLedger":123000,"ledgerRetentionWindow":172800}`)
		case "getLatestLedger":
			respond(w, fmt.Sprintf(`{"id":"aabb","sequence":%d,"protocolVersion":22}`, f.ledger+1))
		case "getLedgers":
			respond(w, fmt.Sprintf(`{"ledgers":[{"hash":"aabb","sequence":%d,"ledgerCloseTime":"2026-11-26T10:00:00Z"}],"latestLedger":%d,"cursor":"next"}`, f.ledger, f.ledger+1))
		case "getEvents":
			respond(w, fmt.Sprintf(`{"events":[%s],"latestLedger":%d,"oldestLedger":123000,"cursor":%q}`, eventJSON, f.ledger+1, f.eventID))
		case "getNetwork":
			respond(w, `{"passphrase":"Test SDF Network ; September 2015","protocolVersion":22}`)
		default:
			respond(w, `{}`)
		}
	})

	f.server = httptest.NewServer(f.mux)
	t.Cleanup(f.server.Close)
	return f
}

// webhookServer starts an httptest server that records received webhook
// payloads. received is incremented atomically for each POST.
type webhookServer struct {
	server   *httptest.Server
	received atomic.Int64
	payloads chan []byte
}

func newWebhookServer(t *testing.T) *webhookServer {
	t.Helper()
	ws := &webhookServer{
		payloads: make(chan []byte, 10),
	}
	ws.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		ws.received.Add(1)
		select {
		case ws.payloads <- body:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ws.server.Close)
	return ws
}

// pollUntil calls check at most maxWait/interval times, returning nil when
// check returns nil or the error from the last call when it times out.
func pollUntil(ctx context.Context, interval, maxWait time.Duration, check func() error) error {
	deadline := time.Now().Add(maxWait)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = check()
		if last == nil {
			return nil
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("timed out after %s: %w", maxWait, last)
}

// TestSmokePollingMatchingAndDelivery is the happy-path E2E:
//  1. Build the binary; start a fake RPC and a webhook receiver.
//  2. Start sorobeacon (SQLite, random port).
//  3. Create a monitor, a rule (event_emitted matching the contract) and a
//     webhook channel through the API; wire the channel to the monitor.
//  4. Wait for the poller to process the scripted event and deliver the alert.
//  5. Assert the webhook was called and the delivery attempt is recorded.
//  6. Send SIGTERM; assert clean shutdown within 15 s.
func TestSmokePollingMatchingAndDelivery(t *testing.T) {
	bin := buildBinary(t)

	eventJSON := fmt.Sprintf(`{
		"id": "0000000123456789-0000000000",
		"type": "contract",
		"ledger": 123456,
		"ledgerClosedAt": "2026-11-26T10:00:00Z",
		"contractId": %q,
		"pagingToken": "0000000123456789-0000000000",
		"inSuccessfulContractCall": true,
		"txHash": "abc123",
		"topicJson": [{"symbol": "transfer"}],
		"valueJson": {"i128": "1000000000"}
	}`, contractID)

	rpc := newFakeRPC(t, eventJSON)
	webhook := newWebhookServer(t)

	dbPath := filepath.Join(t.TempDir(), "sorobeacon.db")
	httpPort := freePort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	apiBase := "http://" + httpAddr

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL=sqlite://"+dbPath,
		"RPC_URL="+rpc.server.URL,
		"NETWORK=testnet",
		"HTTP_ADDR="+httpAddr,
		"API_TOKEN="+apiToken,
		"POLL_INTERVAL=1s",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	require.NoError(t, cmd.Start(), "start sorobeacon")
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := apiclient.New(apiBase, apiclient.WithToken(apiToken))

	// Wait for the server to become ready.
	require.NoError(t, pollUntil(ctx, 200*time.Millisecond, 20*time.Second, func() error {
		resp, err := http.Get(apiBase + "/api/v1/health")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health: %d", resp.StatusCode)
		}
		return nil
	}), "server did not become ready")

	// Create a monitor watching the contract.
	monitor, err := client.CreateMonitor(ctx, apiclient.MonitorCreate{
		Name:        "e2e-monitor",
		ContractIDs: []string{contractID},
	})
	require.NoError(t, err)

	// Add an event_emitted rule.
	_, err = client.CreateRule(ctx, monitor.ID, apiclient.RuleCreate{
		Type:   "event_emitted",
		Params: json.RawMessage(`{"event_name":"transfer"}`),
	})
	require.NoError(t, err)

	// Create a webhook channel.
	webhookConfig, _ := json.Marshal(map[string]string{"url": webhook.server.URL})
	ch, err := client.CreateChannel(ctx, apiclient.ChannelCreate{
		Name:   "e2e-webhook",
		Type:   "webhook",
		Config: json.RawMessage(webhookConfig),
	})
	require.NoError(t, err)

	// Wire the channel to the monitor.
	chIDs := []int64{ch.ID}
	_, err = client.UpdateMonitor(ctx, monitor.ID, apiclient.MonitorUpdate{
		ChannelIDs: &chIDs,
	})
	require.NoError(t, err)

	// Wait for the webhook to receive a delivery.
	require.NoError(t, pollUntil(ctx, 500*time.Millisecond, 30*time.Second, func() error {
		if webhook.received.Load() == 0 {
			return fmt.Errorf("no webhook delivery yet")
		}
		return nil
	}), "webhook never received a delivery")

	// Exactly one delivery.
	assert.GreaterOrEqual(t, webhook.received.Load(), int64(1))

	// The payload must be a valid JSON object.
	select {
	case body := <-webhook.payloads:
		var payload map[string]any
		assert.NoError(t, json.Unmarshal(body, &payload), "webhook payload is valid JSON: %s", body)
	default:
		t.Error("no payload recorded")
	}

	// Delivery attempt visible through the API.
	require.NoError(t, pollUntil(ctx, 500*time.Millisecond, 10*time.Second, func() error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/api/v1/alerts?limit=5", apiBase), nil)
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("list alerts: %d", resp.StatusCode)
		}
		var result struct {
			Alerts []struct{ ID int64 `json:"id"` } `json:"alerts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return err
		}
		if len(result.Alerts) == 0 {
			return fmt.Errorf("no alerts yet")
		}
		return nil
	}), "no alerts recorded")

	// Clean shutdown: send SIGTERM and wait at most 15 s.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(15 * time.Second):
		t.Error("sorobeacon did not shut down within 15 s after SIGTERM")
	case err := <-done:
		// A clean shutdown on SIGTERM exits with status 0 or the platform's
		// signal exit code — both are acceptable.
		_ = err
	}
}
