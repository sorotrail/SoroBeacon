package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/metrics"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testContractID = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA"

// createMonitorStore records CreateMonitor so body-limit tests can tell a
// 413 from a 201 without a database.
type createMonitorStore struct {
	store.Store
	created int
	createdTemplates int
	createdChannels int
}

func (c *createMonitorStore) CreateMonitor(_ context.Context, m *store.Monitor) error {
	c.created++
	m.ID = int64(c.created)
	return nil
}

func (c *createMonitorStore) CreateAuditEntry(_ context.Context, _ *store.AuditEntry) error {
	return nil
}

func (c *createMonitorStore) CreateMonitorTemplate(_ context.Context, t *store.MonitorTemplate) error {
	c.createdTemplates++
	t.ID = int64(c.createdTemplates)
	return nil
}

func (c *createMonitorStore) CreateChannel(_ context.Context, ch *store.Channel) error {
	c.createdChannels++
	ch.ID = int64(c.createdChannels)
	return nil
}

func monitorJSONWithPad(t *testing.T, n int) []byte {
	t.Helper()
	prefix := `{"name":"n","contract_ids":["` + testContractID + `"],"pad":"`
	suffix := `"}`
	need := n - len(prefix) - len(suffix)
	if need < 0 {
		t.Fatalf("target size %d is smaller than the minimum monitor JSON (%d)", n, len(prefix)+len(suffix))
	}
	return []byte(prefix + strings.Repeat("a", need) + suffix)
}

func decodeEnvelope(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env
}

func TestMaxBodyBytes_AcceptedUnderLimit(t *testing.T) {
	const limit = 256
	st := &createMonitorStore{}
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(limit)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := monitorJSONWithPad(t, limit-1)
	res, err := http.Post(srv.URL+"/monitors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST under limit = %d, want 201; body %s", res.StatusCode, slurp(t, res.Body))
	}
	if st.created != 1 {
		t.Fatalf("CreateMonitor called %d times, want 1", st.created)
	}
}

func TestMaxBodyBytes_ExactlyAtLimit(t *testing.T) {
	const limit = 256
	st := &createMonitorStore{}
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(limit)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := monitorJSONWithPad(t, limit)
	res, err := http.Post(srv.URL+"/monitors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST at limit = %d, want 201; body %s", res.StatusCode, slurp(t, res.Body))
	}
	if st.created != 1 {
		t.Fatalf("CreateMonitor called %d times, want 1", st.created)
	}
}

func TestMaxBodyBytes_OversizedReturns413Envelope(t *testing.T) {
	const limit = 256
	st := &createMonitorStore{}
	m := metrics.New()
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(limit).WithMetrics(m)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := monitorJSONWithPad(t, limit+1)
	res, err := http.Post(srv.URL+"/monitors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST over limit = %d, want 413", res.StatusCode)
	}
	env := decodeEnvelope(t, res.Body)
	if env["error"] != "request body too large" {
		t.Fatalf("error = %v, want request body too large", env["error"])
	}
	if env["code"] != apierr.CodePayloadTooLarge {
		t.Fatalf("code = %v, want %s", env["code"], apierr.CodePayloadTooLarge)
	}
	if _, ok := env["request_id"]; !ok {
		t.Fatalf("envelope missing request_id: %v", env)
	}
	if st.created != 0 {
		t.Fatalf("CreateMonitor called %d times, want 0 on 413", st.created)
	}
}

func TestMaxBodyBytes_GETUnaffected(t *testing.T) {
	s := New(&fakeStore{}, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(8)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /livez with tiny body limit = %d, want 200", res.StatusCode)
	}
}

func gzipCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestMaxBodyBytes_GzipDecompressionAccepted(t *testing.T) {
	const limit = 512
	st := &createMonitorStore{}
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(limit)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	raw := monitorJSONWithPad(t, 200)
	compressed := gzipCompress(t, raw)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/monitors", bytes.NewReader(compressed))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusCreated, res.StatusCode)
	assert.Equal(t, 1, st.created)
}

func TestMaxBodyBytes_CompressionBombRejected413(t *testing.T) {
	const limit = 512
	st := &createMonitorStore{}
	m := metrics.New()
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).WithMaxBodyBytes(limit).WithMetrics(m)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	// 50 KiB payload that compresses down to a few hundred bytes
	raw := monitorJSONWithPad(t, 50*1024)
	compressed := gzipCompress(t, raw)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/monitors", bytes.NewReader(compressed))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusRequestEntityTooLarge, res.StatusCode)
	env := decodeEnvelope(t, res.Body)
	assert.Equal(t, "request body too large", env["error"])
	assert.Equal(t, 0, st.created, "compression bomb must never reach CreateMonitor")
}

func TestMaxBodyBytes_EndpointOverride(t *testing.T) {
	st := &createMonitorStore{}
	s := New(st, nil, nil, &fakeRPC{}, discardLogger()).
		WithMaxBodyBytes(256).
		WithBulkMaxBodyBytes(4096).
		WithEndpointMaxBodyBytes("/monitors/custom-large", 2048)

	assert.Equal(t, int64(256), s.limitForRequest(httptest.NewRequest(http.MethodPost, "/monitors", nil)))
	assert.Equal(t, int64(4096), s.limitForRequest(httptest.NewRequest(http.MethodPost, "/monitors/bulk", nil)))
	assert.Equal(t, int64(4096), s.limitForRequest(httptest.NewRequest(http.MethodPost, "/alerts/import", nil)))
	assert.Equal(t, int64(2048), s.limitForRequest(httptest.NewRequest(http.MethodPost, "/monitors/custom-large", nil)))
}

func TestMaxBodyBytes_LargeTemplateRejected(t *testing.T) {
	st := &createMonitorStore{}
	m := metrics.New()
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).WithMetrics(m)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	hugeDescription := strings.Repeat("x", MaxTemplateBytes+10)
	body, _ := json.Marshal(map[string]any{
		"name":        "oversized-template",
		"description": hugeDescription,
	})

	res, err := http.Post(srv.URL+"/templates", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
	env := decodeEnvelope(t, res.Body)
	details, ok := env["details"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, details)
	d0 := details[0].(map[string]any)
	assert.Equal(t, "description", d0["field"])
	assert.Contains(t, d0["reason"], fmt.Sprintf("exceeds maximum size of %d bytes", MaxTemplateBytes))
}

func TestMaxBodyBytes_LargeChannelConfigRejected(t *testing.T) {
	st := &createMonitorStore{}
	m := metrics.New()
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).WithMetrics(m)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	hugeURL := "https://example.com/webhook/" + strings.Repeat("a", MaxChannelConfigBytes+10)
	body, _ := json.Marshal(map[string]any{
		"name": "oversized-channel",
		"type": "webhook",
		"config": map[string]any{
			"url":    hugeURL,
			"secret": "s3cr3t",
		},
	})

	res, err := http.Post(srv.URL+"/channels", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
	env := decodeEnvelope(t, res.Body)
	details, ok := env["details"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, details)
	d0 := details[0].(map[string]any)
	assert.Equal(t, "config", d0["field"])
	assert.Contains(t, d0["reason"], fmt.Sprintf("channel config exceeds maximum size of %d bytes", MaxChannelConfigBytes))
}

func TestMaxBodyBytes_LargeNotificationTemplateInConfigRejected(t *testing.T) {
	st := &createMonitorStore{}
	m := metrics.New()
	s := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger()).WithMetrics(m)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	hugeTemplate := strings.Repeat("t", MaxTemplateBytes+10)
	body, _ := json.Marshal(map[string]any{
		"name": "oversized-template-channel",
		"type": "slack",
		"config": map[string]any{
			"webhook_url": "https://hooks.slack.com/services/T00/B00/X",
			"template":    hugeTemplate,
		},
	})

	res, err := http.Post(srv.URL+"/channels", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
	env := decodeEnvelope(t, res.Body)
	details, ok := env["details"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, details)
	d0 := details[0].(map[string]any)
	assert.Equal(t, "config.template", d0["field"])
	assert.Contains(t, d0["reason"], fmt.Sprintf("template exceeds maximum size of %d bytes", MaxTemplateBytes))
}

func slurp(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
