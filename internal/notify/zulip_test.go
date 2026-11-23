package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zulipRequest is what the test server saw.
type zulipRequest struct {
	method      string
	path        string
	contentType string
	user, pass  string
	authOK      bool
	form        url.Values
}

func zulipServer(t *testing.T, status int, got *zulipRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.contentType = r.Header.Get("Content-Type")
		got.user, got.pass, got.authOK = r.BasicAuth()
		_ = r.ParseForm()
		got.form = r.PostForm
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"result":"error","msg":"nope"}`))
	}))
	t.Cleanup(srv.Close)
	useServerClient(t, srv)
	return srv
}

func zulipConfigJSON(site, topic string) json.RawMessage {
	cfg := map[string]string{
		"site":      site,
		"bot_email": "beacon-bot@example.zulipchat.com",
		"api_key":   "sekrit-api-key",
		"stream":    "alerts",
	}
	if topic != "" {
		cfg["topic"] = topic
	}
	b, _ := json.Marshal(cfg)
	return b
}

func TestNewZulipNamesMissingKeys(t *testing.T) {
	full := map[string]string{
		"site":      "https://example.zulipchat.com",
		"bot_email": "bot@example.com",
		"api_key":   "sekrit-api-key",
		"stream":    "alerts",
	}
	for _, key := range []string{"site", "bot_email", "api_key", "stream"} {
		t.Run(key, func(t *testing.T) {
			cfg := map[string]string{}
			for k, v := range full {
				if k != key {
					cfg[k] = v
				}
			}
			b, _ := json.Marshal(cfg)
			_, err := NewZulip(b)
			require.Error(t, err)
			assert.Equal(t, "zulip: "+key+" is required", err.Error())
			assert.NotContains(t, err.Error(), "sekrit-api-key")
		})
	}

	_, err := NewZulip(json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Equal(t, "zulip: site, bot_email, api_key, stream is required", err.Error())
}

func TestNewZulipRequiresHTTPS(t *testing.T) {
	_, err := NewZulip(zulipConfigJSON("http://example.zulipchat.com", ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "site must use HTTPS")
	assert.NotContains(t, err.Error(), "sekrit-api-key")
}

func TestZulipSendFormAndAuth(t *testing.T) {
	var got zulipRequest
	srv := zulipServer(t, http.StatusOK, &got)

	// A trailing slash on site must not produce a double slash in the path.
	n, err := NewZulip(zulipConfigJSON(srv.URL+"/", "sorobeacon"))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), testChatAlert))

	assert.Equal(t, http.MethodPost, got.method)
	assert.Equal(t, "/api/v1/messages", got.path)
	assert.Equal(t, "application/x-www-form-urlencoded", got.contentType)
	require.True(t, got.authOK, "request must carry HTTP basic auth")
	assert.Equal(t, "beacon-bot@example.zulipchat.com", got.user)
	assert.Equal(t, "sekrit-api-key", got.pass)

	assert.Equal(t, "stream", got.form.Get("type"))
	assert.Equal(t, "alerts", got.form.Get("to"))
	assert.Equal(t, "sorobeacon", got.form.Get("topic"))
	assert.Contains(t, got.form.Get("content"), "treasury-watch")
	assert.Contains(t, got.form.Get("content"), "0000123-0000000001")
}

func TestZulipDefaultTopicIsMonitorName(t *testing.T) {
	var got zulipRequest
	srv := zulipServer(t, http.StatusOK, &got)

	n, err := NewZulip(zulipConfigJSON(srv.URL, ""))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), testChatAlert))
	assert.Equal(t, "treasury-watch", got.form.Get("topic"))
}

func TestZulipTopicFallbackAndTruncation(t *testing.T) {
	var got zulipRequest
	srv := zulipServer(t, http.StatusOK, &got)

	n, err := NewZulip(zulipConfigJSON(srv.URL, ""))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{Digest: "digest"}))
	assert.Equal(t, zulipFallbackTopic, got.form.Get("topic"))

	long := strings.Repeat("m", 100)
	require.NoError(t, n.Send(context.Background(), Alert{MonitorName: long}))
	assert.Equal(t, zulipMaxTopicLen, len([]rune(got.form.Get("topic"))))
}

func TestZulipSendNon2xx(t *testing.T) {
	var got zulipRequest
	srv := zulipServer(t, http.StatusUnauthorized, &got)

	n, err := NewZulip(zulipConfigJSON(srv.URL, ""))
	require.NoError(t, err)
	err = n.Send(context.Background(), testChatAlert)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")
	assert.NotContains(t, err.Error(), "sekrit-api-key")
	assert.NotContains(t, err.Error(), srv.URL)
}

func TestZulipSendNetworkFailureRedacts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	site := srv.URL
	srv.Close()

	n, err := NewZulip(zulipConfigJSON(site, ""))
	require.NoError(t, err)
	err = n.Send(context.Background(), testChatAlert)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "sekrit-api-key")
	assert.NotContains(t, err.Error(), fmt.Sprint(site))
}
