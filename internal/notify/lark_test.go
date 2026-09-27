package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLarkValidConfig(t *testing.T) {
	n, err := NewLark(json.RawMessage(`{"webhook_url": "https://open.larksuite.com/open-apis/bot/v2/hook/xxxx"}`))
	require.NoError(t, err)
	require.NotNil(t, n)
	_, ok := n.(*Lark)
	assert.True(t, ok, "NewLark must return a *Lark")
}

func TestNewLarkWithSecret(t *testing.T) {
	n, err := NewLark(json.RawMessage(`{"webhook_url": "https://open.larksuite.com/open-apis/bot/v2/hook/xxxx", "secret": "mysecret"}`))
	require.NoError(t, err)
	require.NotNil(t, n)
}

func TestNewLarkMalformedConfig(t *testing.T) {
	_, err := NewLark(json.RawMessage(`{"webhook_url": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewLarkMissingWebhookURL(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty webhook_url", `{"webhook_url": ""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewLark(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "webhook_url is required")
		})
	}
}

func TestNewLarkNonHTTPSWebhookURL(t *testing.T) {
	_, err := NewLark(json.RawMessage(`{"webhook_url": "http://open.larksuite.com/open-apis/bot/v2/hook/xxxx"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "webhook_url must use HTTPS")
}

func TestNewLarkInvalidTemplate(t *testing.T) {
	_, err := NewLark(json.RawMessage(
		`{"webhook_url": "https://open.larksuite.com/open-apis/bot/v2/hook/xxxx", "template": "{{.Unclosed"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid template")
}

func TestLarkSign(t *testing.T) {
	// Test with a known timestamp and secret
	secret := "testsecret"
	timestamp := int64(1599360473)
	sign := larkSign(secret, timestamp)
	// Just verify it produces a valid base64 string
	_, err := base64.StdEncoding.DecodeString(sign)
	require.NoError(t, err)
	// Verify it's deterministic
	sign2 := larkSign(secret, timestamp)
	assert.Equal(t, sign, sign2)
}

func TestLarkSendSuccess(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		// Lark returns 200 with code=0 for success
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	n, err := NewLark(json.RawMessage(fmt.Sprintf(`{"webhook_url": "https://example.com/hook", "api_base": %q}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{ID: 7, MonitorName: "my-monitor", EventID: "ev-1"}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)

	var payload struct {
		MsgType string `json:"msg_type"`
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
		Timestamp int64  `json:"timestamp"`
		Sign      string `json:"sign"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "text", payload.MsgType)
	assert.Contains(t, payload.Content.Text, "my-monitor")
	assert.Equal(t, int64(0), payload.Timestamp) // No secret, so no timestamp
	assert.Equal(t, "", payload.Sign)            // No secret, so no sign
}

func TestLarkSendWithSecret(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"webhook_url": "https://example.com/hook", "secret": "mysecret", "api_base": %q}`, srv.URL)
	n, err := NewLark(json.RawMessage(cfg))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, EventID: "ev-42"}))

	var payload struct {
		Timestamp int64  `json:"timestamp"`
		Sign      string `json:"sign"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.NotEqual(t, int64(0), payload.Timestamp)
	assert.NotEqual(t, "", payload.Sign)

	// Verify the signature is correct
	expectedSign := larkSign("mysecret", payload.Timestamp)
	assert.Equal(t, expectedSign, payload.Sign)
}

func TestLarkSendCustomTemplate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"webhook_url": "https://example.com/hook", "api_base": %q, "template": "alert {{.EventID}}"}`, srv.URL)
	n, err := NewLark(json.RawMessage(cfg))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, EventID: "ev-42"}))

	var payload struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "alert ev-42", payload.Content.Text)
}

func TestLarkSendLarkError(t *testing.T) {
	// Lark returns HTTP 200 but with non-zero code for failures
	tests := []struct {
		name       string
		larkCode   int
		larkMsg    string
		wantErrMsg string
	}{
		{"invalid signature", 94100, "signature verification failed", "94100"},
		{"rate limited", 42900, "rate limit exceeded", "42900"},
		{"invalid webhook", 40400, "webhook not found", "40400"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"code":%d,"msg":"%s"}`, tt.larkCode, tt.larkMsg)
			}))
			defer srv.Close()

			n, err := NewLark(json.RawMessage(fmt.Sprintf(`{"webhook_url": "https://example.com/hook", "api_base": %q}`, srv.URL)))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrMsg)
		})
	}
}

func TestLarkSendHTTPError(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"client error", http.StatusBadRequest},
		{"server error", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tt.status)
			}))
			defer srv.Close()

			n, err := NewLark(json.RawMessage(fmt.Sprintf(`{"webhook_url": "https://example.com/hook", "api_base": %q}`, srv.URL)))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", tt.status))
			// Ensure secret is not in error message
			assert.NotContains(t, err.Error(), "mysecret")
		})
	}
}

func TestLarkSendNetworkFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	n, err := NewLark(json.RawMessage(fmt.Sprintf(`{"webhook_url": "https://example.com/hook", "api_base": %q}`, url)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "post")
}

func TestLarkSendResponseMissingCode(t *testing.T) {
	// Lark returns 200 but response doesn't have expected code field
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"msg":"success"}`)) // missing code field
	}))
	defer srv.Close()

	n, err := NewLark(json.RawMessage(fmt.Sprintf(`{"webhook_url": "https://example.com/hook", "api_base": %q}`, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	// This should still succeed since we don't strictly require code field
	// but if the response is not valid JSON or missing, we should handle it
	require.NoError(t, err)
}