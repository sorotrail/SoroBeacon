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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushoverServer records the posted form and answers with status.
func pushoverServer(t *testing.T, status int, path *string, form *url.Values) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*path = r.URL.Path
		_ = r.ParseForm()
		*form = r.PostForm
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":0,"errors":["application token is invalid"]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pushoverConfigJSON(apiBase, extra string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"token": "app-sekrit", "user": "user-sekrit", "api_base": %q%s}`, apiBase, extra))
}

func TestNewPushoverRequiresCredentials(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{"both missing", `{}`, "pushover: token, user is required"},
		{"token missing", `{"user": "user-sekrit"}`, "pushover: token is required"},
		{"user missing", `{"token": "app-sekrit"}`, "pushover: user is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPushover(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Equal(t, tt.want, err.Error())
			assert.NotContains(t, err.Error(), "sekrit")
		})
	}
}

func TestNewPushoverPriorityValidation(t *testing.T) {
	tests := []struct {
		name    string
		extra   string
		want    int
		wantErr bool
	}{
		{"default", ``, 0, false},
		{"lowest", `, "priority": -2`, -2, false},
		{"quiet", `, "priority": -1`, -1, false},
		{"normal", `, "priority": 0`, 0, false},
		{"high", `, "priority": 1`, 1, false},
		{"emergency rejected", `, "priority": 2`, 0, true},
		{"too low", `, "priority": -3`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewPushover(pushoverConfigJSON("https://api.pushover.net", tt.extra))
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "priority must be between -2 and 1")
				assert.NotContains(t, err.Error(), "sekrit")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, n.(*Pushover).priority)
		})
	}
}

func TestPushoverSendForm(t *testing.T) {
	var (
		path string
		form url.Values
	)
	srv := pushoverServer(t, http.StatusOK, &path, &form)

	n, err := NewPushover(pushoverConfigJSON(srv.URL, `, "priority": 1, "device": "phone"`))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), testChatAlert))

	assert.Equal(t, "/1/messages.json", path)
	assert.Equal(t, "app-sekrit", form.Get("token"))
	assert.Equal(t, "user-sekrit", form.Get("user"))
	assert.Equal(t, "1", form.Get("priority"))
	assert.Equal(t, "phone", form.Get("device"))
	assert.Equal(t, "SoroBeacon: treasury-watch", form.Get("title"))
	msg := form.Get("message")
	assert.Contains(t, msg, "event_match")
	assert.Contains(t, msg, "0000123-0000000001")
	assert.Contains(t, msg, "CABC123")
}

func TestPushoverSendOmitsUnsetDevice(t *testing.T) {
	var (
		path string
		form url.Values
	)
	srv := pushoverServer(t, http.StatusOK, &path, &form)

	n, err := NewPushover(pushoverConfigJSON(srv.URL, ""))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), testChatAlert))
	assert.NotContains(t, form, "device")
	assert.Equal(t, "0", form.Get("priority"))
}

func TestPushoverSendTruncates(t *testing.T) {
	var (
		path string
		form url.Values
	)
	srv := pushoverServer(t, http.StatusOK, &path, &form)

	// Multi-byte characters check that limits are counted in characters and
	// that the cut never splits one.
	cfg := pushoverConfigJSON(srv.URL, `, "template": "{{.EventName}}"`)
	n, err := NewPushover(cfg)
	require.NoError(t, err)
	a := Alert{
		MonitorName: strings.Repeat("é", 400),
		EventName:   strings.Repeat("ü", 3000),
	}
	require.NoError(t, n.Send(context.Background(), a))

	title, msg := form.Get("title"), form.Get("message")
	assert.Equal(t, pushoverMaxTitle, utf8.RuneCountInString(title))
	assert.Equal(t, pushoverMaxMessage, utf8.RuneCountInString(msg))
	assert.True(t, utf8.ValidString(title))
	assert.True(t, utf8.ValidString(msg))
	assert.True(t, strings.HasSuffix(msg, "…"))
}

func TestPushoverSendNon2xx(t *testing.T) {
	var (
		path string
		form url.Values
	)
	srv := pushoverServer(t, http.StatusBadRequest, &path, &form)

	n, err := NewPushover(pushoverConfigJSON(srv.URL, ""))
	require.NoError(t, err)
	err = n.Send(context.Background(), testChatAlert)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
	assert.NotContains(t, err.Error(), "app-sekrit")
	assert.NotContains(t, err.Error(), "user-sekrit")
}

func TestTruncateRunes(t *testing.T) {
	assert.Equal(t, "abc", truncateRunes("abc", 3))
	assert.Equal(t, "ab…", truncateRunes("abcd", 3))
	assert.Equal(t, "", truncateRunes("abc", 0))
}
