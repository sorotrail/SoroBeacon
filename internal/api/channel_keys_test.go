package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAPIServerWithSQLite(t *testing.T) (*Server, store.Store, func()) {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "api_keys.db")
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)

	srv := New(st, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	return srv, st, func() { st.Close() }
}

func TestAPI_ChannelSigningKeyRotationLifecycle(t *testing.T) {
	srv, st, cleanup := newTestAPIServerWithSQLite(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Create a webhook channel
	ch := &store.Channel{
		Name:    "webhook-sink",
		Type:    "webhook",
		Config:  json.RawMessage(`{"url": "https://example.com/alerts", "secret": "initial-secret"}`),
		Enabled: true,
	}
	require.NoError(t, st.CreateChannel(ctx, ch))

	mux := srv.Routes()

	// 2. Rotate key via API
	rotateBody := `{"key_id":"csk_v2","secret":"my-new-secret-32-chars-long-1234"}`
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/channels/%d/keys/rotate", ch.ID), bytes.NewReader([]byte(rotateBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	var rotResp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rotResp))
	assert.Equal(t, "csk_v2", rotResp["key_id"])
	assert.Equal(t, "my-new-secret-32-chars-long-1234", rotResp["secret"])
	assert.Equal(t, true, rotResp["is_current"])
	assert.Equal(t, "active", rotResp["status"])

	// 3. List keys via API - secret MUST NOT be present
	reqList := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/channels/%d/keys", ch.ID), nil)
	wList := httptest.NewRecorder()
	mux.ServeHTTP(wList, reqList)

	require.Equal(t, http.StatusOK, wList.Code)
	var listResp struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(wList.Body.Bytes(), &listResp))
	require.NotEmpty(t, listResp.Keys)

	for _, k := range listResp.Keys {
		assert.Nil(t, k["secret"], "secrets must NEVER appear in key listing API responses")
	}

	// 4. Cannot retire the current key
	reqRetireCurr := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/channels/%d/keys/csk_v2/retire", ch.ID), nil)
	wRetireCurr := httptest.NewRecorder()
	mux.ServeHTTP(wRetireCurr, reqRetireCurr)
	assert.Equal(t, http.StatusBadRequest, wRetireCurr.Code)

	// 5. Rotate again to generate a new key
	reqRot2 := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/channels/%d/rotate-signing-key", ch.ID), bytes.NewReader([]byte(`{}`)))
	wRot2 := httptest.NewRecorder()
	mux.ServeHTTP(wRot2, reqRot2)
	require.Equal(t, http.StatusCreated, wRot2.Code)
	var rot2Resp map[string]any
	require.NoError(t, json.Unmarshal(wRot2.Body.Bytes(), &rot2Resp))
	newKeyID := rot2Resp["key_id"].(string)
	assert.NotEmpty(t, newKeyID)
	assert.NotEqual(t, "csk_v2", newKeyID)

	// 6. Now retire csk_v2 (since it is no longer current)
	reqRetire := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/channels/%d/keys/csk_v2/retire", ch.ID), nil)
	wRetire := httptest.NewRecorder()
	mux.ServeHTTP(wRetire, reqRetire)
	require.Equal(t, http.StatusOK, wRetire.Code)

	// 7. Emergency revocation of compromised new key
	reqRevoke := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/channels/%d/keys/%s/revoke", ch.ID, newKeyID), nil)
	wRevoke := httptest.NewRecorder()
	mux.ServeHTTP(wRevoke, reqRevoke)
	require.Equal(t, http.StatusOK, wRevoke.Code)
	var revokeResp map[string]any
	require.NoError(t, json.Unmarshal(wRevoke.Body.Bytes(), &revokeResp))
	assert.Equal(t, "revoked", revokeResp["status"])
}

func TestAPI_ChannelSigningKeys_Scopes(t *testing.T) {
	readScopes, ok := requiredScopes(http.MethodGet, "/channels/1/keys")
	require.True(t, ok)
	assert.Contains(t, readScopes, auth.ScopeChannelsRead)

	rotateScopes, ok := requiredScopes(http.MethodPost, "/channels/1/keys/rotate")
	require.True(t, ok)
	assert.Contains(t, rotateScopes, auth.ScopeChannelsWrite)

	retireScopes, ok := requiredScopes(http.MethodPost, "/channels/1/keys/k1/retire")
	require.True(t, ok)
	assert.Contains(t, retireScopes, auth.ScopeChannelsWrite)

	revokeScopes, ok := requiredScopes(http.MethodPost, "/channels/1/keys/k1/revoke")
	require.True(t, ok)
	assert.Contains(t, revokeScopes, auth.ScopeChannelsWrite)
}
