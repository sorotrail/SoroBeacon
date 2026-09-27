package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatrixSendsThroughClientServerAPI(t *testing.T) {
	var (
		mu      sync.Mutex
		methods []string
		paths   []string
		bodies  [][]byte
		auths   []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, body)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		fmt.Fprint(w, `{"event_id":"$1"}`)
	}))
	defer srv.Close()

	const token = "syt_super-secret-token"
	n, err := NewMatrix(json.RawMessage(fmt.Sprintf(
		`{"homeserver_url": %q, "access_token": %q, "room_id": "!room:example.org"}`, srv.URL, token)))
	require.NoError(t, err)

	alert := Alert{ID: 42, MonitorName: "m1", EventID: "ev-1"}
	require.NoError(t, n.Send(context.Background(), alert))
	// A retry of the same alert (e.g. the dispatcher backing off and trying
	// again) must reuse the transaction ID, which is what makes the PUT
	// idempotent on the homeserver.
	require.NoError(t, n.Send(context.Background(), alert))

	require.Len(t, paths, 2)
	assert.Equal(t, http.MethodPut, methods[0])
	wantPath := "/_matrix/client/v3/rooms/!room:example.org/send/m.room.message/42"
	assert.Equal(t, wantPath, paths[0])
	assert.Equal(t, wantPath, paths[1], "the transaction ID must be stable across retries")
	for _, got := range auths {
		assert.Equal(t, "Bearer "+token, got)
	}

	var msg map[string]string
	require.NoError(t, json.Unmarshal(bodies[0], &msg))
	assert.Equal(t, "m.text", msg["msgtype"])
	assert.Contains(t, msg["body"], "m1")
}

func TestMatrixEscapesRoomAndTransactionID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
	}))
	defer srv.Close()

	n, err := NewMatrix(json.RawMessage(fmt.Sprintf(
		`{"homeserver_url": %q, "access_token": "t", "room_id": "!abc:example.org"}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 7}))

	assert.True(t, strings.Contains(gotPath, "%21abc"), "room id bang must be path-escaped: %s", gotPath)
	assert.True(t, strings.HasSuffix(gotPath, "/7"), "transaction id must be the last segment: %s", gotPath)
}

func TestMatrixNon2xxIsErrorWithoutToken(t *testing.T) {
	const token = "syt_do-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	n, err := NewMatrix(json.RawMessage(fmt.Sprintf(
		`{"homeserver_url": %q, "access_token": %q, "room_id": "!r:x"}`, srv.URL, token)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.NotContains(t, err.Error(), token, "errors must never leak the access token")
}

func TestMatrixConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{"nothing set", `{}`, true},
		{"no room", `{"homeserver_url": "https://m.example.org", "access_token": "t"}`, true},
		{"no token", `{"homeserver_url": "https://m.example.org", "room_id": "!r:x"}`, true},
		{"no homeserver", `{"access_token": "t", "room_id": "!r:x"}`, true},
		{"complete", `{"homeserver_url": "https://m.example.org", "access_token": "t", "room_id": "!r:x"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMatrix(json.RawMessage(tt.config))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
