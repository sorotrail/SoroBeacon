package notify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testHTTPClient returns an HTTP client that skips TLS verification for testing.
func testHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// TestDingTalkSignGoldenVector pins the canonical signature for a fixed
// secret and timestamp. Receivers reimplementing verification in another
// language can check themselves against these exact bytes:
//
//	base64(hmac_sha256(key="SECabc123", msg="1700000000000\nSECabc123"))
func TestDingTalkSignGoldenVector(t *testing.T) {
	const want = "N5P09a4+p1AMJIJWnIvQd2Yxw9+fu/oEBnPrjCcsLXk="
	got := dingtalkSign("SECabc123", "1700000000000")
	assert.Equal(t, want, got)
}

func TestNewDingTalkValidConfig(t *testing.T) {
	n, err := NewDingTalk(json.RawMessage(`{"webhook_url": "https://oapi.dingtalk.com/robot/send?access_token=abc"}`))
	require.NoError(t, err)
	require.NotNil(t, n)
	_, ok := n.(*DingTalk)
	assert.True(t, ok, "NewDingTalk must return a *DingTalk")
}

func TestNewDingTalkValidConfigWithSecret(t *testing.T) {
	n, err := NewDingTalk(json.RawMessage(`{"webhook_url": "https://oapi.dingtalk.com/robot/send?access_token=abc", "secret": "SECabc123"}`))
	require.NoError(t, err)
	require.NotNil(t, n)
}

func TestNewDingTalkMalformedConfig(t *testing.T) {
	_, err := NewDingTalk(json.RawMessage(`{"webhook_url": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewDingTalkMissingWebhookURL(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty webhook_url", `{"webhook_url": ""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewDingTalk(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "webhook_url is required")
		})
	}
}

func TestNewDingTalkNonHTTPSWebhookURL(t *testing.T) {
	_, err := NewDingTalk(json.RawMessage(`{"webhook_url": "http://example.com/hook"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must use HTTPS")
}

func TestNewDingTalkInvalidTemplate(t *testing.T) {
	_, err := NewDingTalk(json.RawMessage(
		`{"webhook_url": "https://oapi.dingtalk.com/robot/send?access_token=abc", "template": "{{.Unclosed"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid template")
}

func TestDingTalkSendSuccessWithoutSecret(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
		gotURL         string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	// Use test HTTP client that skips TLS verification
	origClient := httpClient
	httpClient = testHTTPClient()
	defer func() { httpClient = origClient }()

	n, err := NewDingTalk(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{ID: 7, MonitorName: "my-monitor", EventID: "ev-1"}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)
	assert.NotContains(t, gotURL, "timestamp")
	assert.NotContains(t, gotURL, "sign")

	var payload struct {
		MsgType  string                 `json:"msgtype"`
		Markdown map[string]string      `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "markdown", payload.MsgType)
	assert.Equal(t, "SoroBeacon Alert", payload.Markdown["title"])
	assert.Contains(t, payload.Markdown["text"], "my-monitor")
}

func TestDingTalkSendSuccessWithSecret(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
		gotURL         string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"webhook_url": %q, "secret": "SECabc123"}`, srv.URL)
	n, err := NewDingTalk(json.RawMessage(cfg))
	require.NoError(t, err)

	alert := Alert{ID: 7, MonitorName: "my-monitor", EventID: "ev-1"}

	// Use test HTTP client that skips TLS verification
	origClient := httpClient
	httpClient = testHTTPClient()
	defer func() { httpClient = origClient }()

	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)
	assert.Contains(t, gotURL, "timestamp")
	assert.Contains(t, gotURL, "sign")

	// Verify the signature matches our computation
	parsedURL, _ := url.Parse(gotURL)
	query := parsedURL.Query()
	timestamp := query.Get("timestamp")
	sign := query.Get("sign")
	require.NotEmpty(t, timestamp, "timestamp query param must be present")
	require.NotEmpty(t, sign, "sign query param must be present")
	assert.Equal(t, dingtalkSign("SECabc123", timestamp), sign)

	var payload struct {
		MsgType  string            `json:"msgtype"`
		Markdown map[string]string `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "markdown", payload.MsgType)
	assert.Contains(t, payload.Markdown["text"], "my-monitor")
}

func TestDingTalkSendCustomTemplate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"webhook_url": %q, "template": "alert {{.EventID}}"}`, srv.URL)
	n, err := NewDingTalk(json.RawMessage(cfg))
	require.NoError(t, err)

	origClient := httpClient
	httpClient = testHTTPClient()
	defer func() { httpClient = origClient }()

	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, EventID: "ev-42"}))

	var payload struct {
		Markdown map[string]string `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "alert ev-42", payload.Markdown["text"])
}

func TestDingTalkSendServerError(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"client error", http.StatusBadRequest},
		{"server error", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tt.status)
			}))
			defer srv.Close()

			n, err := NewDingTalk(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
			require.NoError(t, err)

			origClient := httpClient
			httpClient = testHTTPClient()
			defer func() { httpClient = origClient }()

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", tt.status))
		})
	}
}

func TestDingTalkSendNetworkFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	n, err := NewDingTalk(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, url)))
	require.NoError(t, err)

	origClient := httpClient
	httpClient = testHTTPClient()
	defer func() { httpClient = origClient }()

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	require.Error(t, err)
}

func TestDingTalkSendDingTalkErrorCode(t *testing.T) {
	// DingTalk returns HTTP 200 with non-zero errcode on failure
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":310000,"errmsg":"sign not match"}`))
	}))
	defer srv.Close()

	n, err := NewDingTalk(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)

	origClient := httpClient
	httpClient = testHTTPClient()
	defer func() { httpClient = origClient }()

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "310000")
	assert.Contains(t, err.Error(), "sign not match")
}

func TestDingTalkSendDingTalkErrorCodeWithSecret(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":310000,"errmsg":"sign not match"}`))
	}))
	defer srv.Close()

	n, err := NewDingTalk(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q, "secret": "SECabc123"}`, srv.URL)))
	require.NoError(t, err)

	origClient := httpClient
	httpClient = testHTTPClient()
	defer func() { httpClient = origClient }()

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "310000")
	assert.Contains(t, err.Error(), "sign not match")
}

func TestDingTalkErrorDoesNotLeakSecrets(t *testing.T) {
	const secret = "SECsuper-secret-do-not-leak"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	n, err := NewDingTalk(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q, "secret": %q}`, srv.URL, secret)))
	require.NoError(t, err)

	origClient := httpClient
	httpClient = testHTTPClient()
	defer func() { httpClient = origClient }()

	err = n.Send(context.Background(), Alert{ID: 1})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), srv.URL, "errors must not leak the webhook URL")
	assert.NotContains(t, err.Error(), secret, "errors must not leak the secret")
}