package notify

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type memDevices struct {
	mu      sync.Mutex
	devs    []store.PushDevice
	retired map[string]bool
}

func (m *memDevices) ListPushDevices(_ context.Context, ch int64) ([]store.PushDevice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.PushDevice
	for _, d := range m.devs {
		if d.ChannelID == ch && !m.retired[d.DeviceID] {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *memDevices) RetirePushDevice(_ context.Context, _ int64, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retired == nil {
		m.retired = map[string]bool{}
	}
	m.retired[id] = true
	return nil
}

func testPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

type pushFixture struct {
	push      *Push
	devices   *memDevices
	fcmBodies []map[string]any
	tokenHits int
	mu        sync.Mutex
}

func newPushFixture(t *testing.T, tokens ...string) *pushFixture {
	t.Helper()
	fx := &pushFixture{devices: &memDevices{}}
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "urn:ietf:params:oauth:grant-type:jwt-bearer", r.Form.Get("grant_type"))
		assert.Equal(t, 3, len(strings.Split(r.Form.Get("assertion"), ".")), "assertion is a signed JWT")
		fx.mu.Lock()
		fx.tokenHits++
		fx.mu.Unlock()
		_, _ = w.Write([]byte(`{"access_token":"ya29.fake","expires_in":3600}`))
	}))
	t.Cleanup(oauth.Close)
	fcm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer ya29.fake", r.Header.Get("Authorization"))
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		fx.mu.Lock()
		fx.fcmBodies = append(fx.fcmBodies, body)
		fx.mu.Unlock()
		tok := body["message"].(map[string]any)["token"].(string)
		if strings.HasPrefix(tok, "dead") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}],"message":"echo ` + tok + `"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"projects/p/messages/1"}`))
	}))
	t.Cleanup(fcm.Close)
	cfg, _ := json.Marshal(map[string]string{
		"project_id": "p", "client_email": "svc@p.iam.gserviceaccount.com", "private_key": testPEM(t),
		"token_url": oauth.URL, "endpoint": fcm.URL, "link_base_url": "https://beacon.example.com/",
	})
	shared := newPushShared()
	shared.devices = fx.devices
	n, err := newPush(cfg, shared)
	require.NoError(t, err)
	fx.push = n.(channelBinder).bind(7, "default").(*Push)
	for i, tok := range tokens {
		fx.devices.devs = append(fx.devices.devs, store.PushDevice{ChannelID: 7, DeviceID: "d" + string(rune('a'+i)), Platform: "fcm", Token: tok})
	}
	return fx
}

func TestPushSendsToRegisteredDevicesAndReusesAccessToken(t *testing.T) {
	fx := newPushFixture(t, "tok-one", "tok-two")
	a := Alert{ID: 9, MonitorName: "Treasury", EventName: "transfer", ContractID: "CABCDEFGHIJKLMNOPQRSTUVWXYZ", Ledger: 42, Severity: "warning"}
	require.NoError(t, fx.push.Send(context.Background(), a))
	require.NoError(t, fx.push.Send(context.Background(), a))
	assert.Len(t, fx.fcmBodies, 4)
	assert.Equal(t, 1, fx.tokenHits, "the access token is cached across deliveries")
	msg := fx.fcmBodies[0]["message"].(map[string]any)
	assert.Equal(t, "https://beacon.example.com/alerts/9", msg["data"].(map[string]any)["link"])
	assert.Equal(t, "Treasury", msg["notification"].(map[string]any)["title"])
}

func TestPushRetiresInvalidTokenAndKeepsDeliveryOk(t *testing.T) {
	fx := newPushFixture(t, "dead-token", "good-token")
	require.NoError(t, fx.push.Send(context.Background(), Alert{ID: 1, MonitorName: "m"}))
	assert.True(t, fx.devices.retired["da"], "the UNREGISTERED device is retired")
	assert.False(t, fx.devices.retired["db"])
	// The next delivery no longer tries the retired device.
	fx.fcmBodies = nil
	require.NoError(t, fx.push.Send(context.Background(), Alert{ID: 2, MonitorName: "m"}))
	assert.Len(t, fx.fcmBodies, 1)
}

func TestPushAllTokensInvalidFailsWithoutLeakingToken(t *testing.T) {
	fx := newPushFixture(t, "dead-secret-token-value")
	err := fx.push.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "dead-secret-token-value")
	assert.NotContains(t, err.Error(), "BEGIN PRIVATE KEY")
	assert.Contains(t, err.Error(), "retired")
}

func TestPushSeverityMapsToDeliveryClass(t *testing.T) {
	crit := PushPriorityFor("critical")
	assert.Equal(t, "critical", crit.InterruptionLevel)
	assert.True(t, crit.Critical)
	warn := PushPriorityFor("")
	assert.Equal(t, "time-sensitive", warn.InterruptionLevel)
	assert.False(t, warn.Critical, "only critical alerts may break through a silenced phone")
	info := PushPriorityFor("info")
	assert.Equal(t, "normal", info.Android)
	assert.Equal(t, "5", info.APNsPriority)

	fx := newPushFixture(t, "tok")
	require.NoError(t, fx.push.Send(context.Background(), Alert{ID: 1, MonitorName: "m", Severity: "critical"}))
	aps := fx.fcmBodies[0]["message"].(map[string]any)["apns"].(map[string]any)["payload"].(map[string]any)["aps"].(map[string]any)
	assert.Equal(t, "critical", aps["interruption-level"])
	assert.Contains(t, aps, "sound")
}

func TestPushPayloadIsSummarisedWithinLimit(t *testing.T) {
	fx := newPushFixture(t, "tok")
	huge := strings.Repeat("x", 20000)
	body, err := fx.push.buildPushMessage(Alert{ID: 1, MonitorName: huge, EventName: huge, ContractID: huge, Payload: json.RawMessage(`{"a":"` + huge + `"}`)}, "tok")
	require.NoError(t, err)
	assert.Less(t, len(body), pushMaxPayload)
	assert.NotContains(t, string(body), `"a":`, "the event payload is never copied into the push")
}

func TestPushRateLimitsPerDevice(t *testing.T) {
	fx := newPushFixture(t, "tok")
	var sendErr error
	for i := 0; i < pushDeviceBurst+3; i++ {
		sendErr = fx.push.Send(context.Background(), Alert{ID: int64(i + 1), MonitorName: "m"})
	}
	require.Error(t, sendErr)
	assert.Contains(t, sendErr.Error(), "rate limited")
	assert.Len(t, fx.fcmBodies, pushDeviceBurst)
}

func TestPushRejectsBadConfig(t *testing.T) {
	_, err := NewPush(json.RawMessage(`{"project_id":"p"}`))
	require.Error(t, err)
	_, err = NewPush(json.RawMessage(`{"project_id":"p","client_email":"e","private_key":"not pem secret-material"}`))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-material")
}
