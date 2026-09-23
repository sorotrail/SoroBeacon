package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewNtfyDefaultsServerURL(t *testing.T) {
	n, err := NewNtfy(json.RawMessage(`{"topic": "ops"}`))
	require.NoError(t, err)
	got, ok := n.(*Ntfy)
	require.True(t, ok)
	assert.Equal(t, defaultNtfyServer, got.cfg.ServerURL)
	assert.Equal(t, "ops", got.cfg.Topic)
	assert.Nil(t, got.cfg.Priority)
}

func TestNewNtfyTrimsTrailingSlash(t *testing.T) {
	n, err := NewNtfy(json.RawMessage(`{"server_url": "https://ntfy.example.com/", "topic": "ops"}`))
	require.NoError(t, err)
	got := n.(*Ntfy)
	assert.Equal(t, "https://ntfy.example.com", got.cfg.ServerURL)
	assert.Equal(t, "https://ntfy.example.com/ops", got.endpoint())
}

func TestNewNtfyAllowsHTTP(t *testing.T) {
	n, err := NewNtfy(json.RawMessage(`{"server_url": "http://ntfy.lan:80", "topic": "ops"}`))
	require.NoError(t, err)
	assert.Equal(t, "http://ntfy.lan:80", n.(*Ntfy).cfg.ServerURL)
}

func TestNewNtfyConfigValidation(t *testing.T) {
	_, err := NewNtfy(json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "topic")

	_, err = NewNtfy(json.RawMessage(`{"topic": "a/b"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "topic")

	_, err = NewNtfy(json.RawMessage(`{"topic": "ops", "server_url": "ftp://ntfy.sh"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http")

	p := 9
	body, err := json.Marshal(ntfyConfig{Topic: "ops", Priority: &p})
	require.NoError(t, err)
	_, err = NewNtfy(body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "priority")
}

func TestNtfyPostsRenderedAlert(t *testing.T) {
	var gotPath, gotTitle, gotAuth, gotPrio, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("Title")
		gotAuth = r.Header.Get("Authorization")
		gotPrio = r.Header.Get("Priority")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	prio := 4
	cfg, err := json.Marshal(ntfyConfig{
		ServerURL:   srv.URL,
		Topic:       "ops-alerts",
		AccessToken: "tok-secret",
		Priority:    &prio,
	})
	require.NoError(t, err)
	n, err := NewNtfy(cfg)
	require.NoError(t, err)

	alert := Alert{
		ID:          7,
		MonitorName: "Token treasury",
		RuleID:      3,
		RuleType:    "value_threshold",
		EventID:     "e1",
		CreatedAt:   time.Date(2026, 7, 21, 9, 41, 32, 0, time.UTC),
	}
	require.NoError(t, n.Send(context.Background(), alert))

	want, err := RenderText(alert)
	require.NoError(t, err)
	assert.Equal(t, "/ops-alerts", gotPath)
	assert.Equal(t, "Token treasury", gotTitle)
	assert.Equal(t, "Bearer tok-secret", gotAuth)
	assert.Equal(t, "4", gotPrio)
	assert.True(t, strings.HasPrefix(gotCT, "text/plain"))
	assert.Equal(t, want, gotBody)
}

func TestNtfyOmitsOptionalHeaders(t *testing.T) {
	var gotAuth, gotPrio string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPrio = r.Header.Get("Priority")
	}))
	defer srv.Close()

	n, err := NewNtfy(json.RawMessage(fmt.Sprintf(`{"server_url": %q, "topic": "ops"}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{MonitorName: "m"}))
	assert.Empty(t, gotAuth)
	assert.Empty(t, gotPrio)
}

func TestNtfyNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	n, err := NewNtfy(json.RawMessage(fmt.Sprintf(
		`{"server_url": %q, "topic": "ops", "access_token": "tok-secret"}`, srv.URL,
	)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "m"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.NotContains(t, err.Error(), "tok-secret")
	assert.NotContains(t, err.Error(), srv.URL)
}
