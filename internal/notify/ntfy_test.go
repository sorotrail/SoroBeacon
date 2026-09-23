package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNtfyDefaultsAndNormalisation(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		wantBase string
		wantPrio string
	}{
		{
			name:     "server url defaults to ntfy.sh",
			config:   `{"topic":"alerts"}`,
			wantBase: "https://ntfy.sh",
		},
		{
			name:     "trailing slash is normalised",
			config:   `{"server_url":"http://ntfy.internal/","topic":"alerts"}`,
			wantBase: "http://ntfy.internal",
		},
		{
			name:     "plain http is permitted for self-hosted",
			config:   `{"server_url":"http://10.0.0.5:2586","topic":"alerts"}`,
			wantBase: "http://10.0.0.5:2586",
		},
		{
			name:     "numeric priority",
			config:   `{"topic":"alerts","priority":5}`,
			wantBase: "https://ntfy.sh",
			wantPrio: "5",
		},
		{
			name:     "named priority maps to its level",
			config:   `{"topic":"alerts","priority":"urgent"}`,
			wantBase: "https://ntfy.sh",
			wantPrio: "5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewNtfy(json.RawMessage(tt.config))
			require.NoError(t, err)
			nt, ok := n.(*Ntfy)
			require.True(t, ok)
			assert.Equal(t, tt.wantBase, nt.baseURL)
			assert.Equal(t, tt.wantPrio, nt.priority)
		})
	}
}

func TestNtfyConfigValidation(t *testing.T) {
	bad := map[string]string{
		`{}`:                               "topic is required",
		`{"topic":""}`:                     "topic is required",
		`{"topic":"a","priority":0}`:       "out of range",
		`{"topic":"a","priority":6}`:       "out of range",
		`{"topic":"a","priority":"noisy"}`: "out of range",
		`{"topic":"a","priority":"2.5"}`:   "out of range",
		`{"topic":"a","priority":{"x":1}}`: "must be a number",
	}
	for cfg, want := range bad {
		_, err := NewNtfy(json.RawMessage(cfg))
		require.Error(t, err, "config %s", cfg)
		assert.Contains(t, err.Error(), want, "config %s", cfg)
	}
}

func TestNtfySend(t *testing.T) {
	var gotPath, gotTitle, gotAuth, gotPriority, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("Title")
		gotAuth = r.Header.Get("Authorization")
		gotPriority = r.Header.Get("Priority")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"server_url":%q,"topic":"my-topic","access_token":"tk_secret","priority":4}`, srv.URL)
	n, err := NewNtfy(json.RawMessage(cfg))
	require.NoError(t, err)

	alert := Alert{ID: 1, MonitorName: "treasury", EventID: "e1"}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, "/my-topic", gotPath)
	assert.Equal(t, "treasury", gotTitle, "Title must carry the monitor name")
	assert.Equal(t, "Bearer tk_secret", gotAuth)
	assert.Equal(t, "4", gotPriority)
	assert.Contains(t, gotCT, "text/plain")

	want, err := RenderText(alert)
	require.NoError(t, err)
	assert.Equal(t, want, gotBody, "the rendered alert is the request body")
}

func TestNtfyWithoutTokenOmitsAuthorization(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	n, err := NewNtfy(json.RawMessage(fmt.Sprintf(`{"server_url":%q,"topic":"t"}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{MonitorName: "m"}))
	assert.Empty(t, gotAuth, "no token configured means no Authorization header")
}

func TestNtfyNon2xxIsErrorWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	n, err := NewNtfy(json.RawMessage(fmt.Sprintf(`{"server_url":%q,"topic":"t","access_token":"tk_secret"}`, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403", "error must include the status code")
	assert.NotContains(t, err.Error(), "tk_secret", "the token must never appear in errors")
}

func TestNtfyRegistered(t *testing.T) {
	_, err := DefaultFactory().New(TypeNtfy, json.RawMessage(`{"topic":"t"}`))
	require.NoError(t, err)
}
