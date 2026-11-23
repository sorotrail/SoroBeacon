package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

type pagingFixture struct {
	st      *store.SQLite
	sync    *PagingSync
	pd      *httptest.Server
	mu      sync.Mutex
	calls   []map[string]string
	chID    int64
	alertID int64
	alert   Alert
	secret  string
	handler http.Handler
}

func newPagingFixture(t *testing.T) *pagingFixture {
	t.Helper()
	fx := &pagingFixture{secret: "whsec-test"}
	url := "sqlite://" + filepath.Join(t.TempDir(), "paging.db")
	require.NoError(t, store.Migrate(url))
	st, err := store.NewSQLite(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	fx.st = st

	fx.pd = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		fx.mu.Lock()
		fx.calls = append(fx.calls, body)
		fx.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(fx.pd.Close)

	ctx := context.Background()
	cfg, _ := json.Marshal(map[string]string{"routing_key": "RK-secret", "events_url": fx.pd.URL, "webhook_secret": fx.secret})
	ch := &store.Channel{Name: "pd", Type: TypePagerDuty, Config: cfg, Enabled: true}
	require.NoError(t, st.CreateChannel(ctx, ch))
	m := &store.Monitor{Name: "m", ContractIDs: []string{"C"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	r := &store.Rule{MonitorID: m.ID, Type: "event_emitted", Params: json.RawMessage(`{}`), Enabled: true}
	require.NoError(t, st.CreateRule(ctx, r))
	al := &store.Alert{MonitorID: m.ID, RuleID: r.ID, EventID: "ev-1"}
	_, err = st.CreateAlert(ctx, al)
	require.NoError(t, err)

	fx.chID, fx.alertID = ch.ID, al.ID
	fx.alert = Alert{ID: al.ID, RuleID: r.ID, EventID: "ev-1"}
	fx.sync = NewPagingSync(st, DefaultFactory(), slog.New(slog.DiscardHandler))
	fx.sync.RecordDelivery(ctx, *ch, fx.alert)
	fx.handler = fx.sync.WebhookHandler("/hooks/pagerduty/")
	return fx
}

func (fx *pagingFixture) post(t *testing.T, event string, secret string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"event":{"event_type":%q,"agent":{"summary":"Alice\u0007"},"data":{"id":"PINC1","incident_key":%q}}}`, event, dedupKey(fx.alert))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/hooks/pagerduty/%s/%d", workspace.Default, fx.chID), bytes.NewBufferString(body))
	req.Header.Set("X-PagerDuty-Signature", "v1=00ff,v1="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	fx.handler.ServeHTTP(rec, req)
	return rec
}

func (fx *pagingFixture) ref(t *testing.T) store.AlertExternalRef {
	t.Helper()
	refs, err := fx.st.ListAlertExternalRefs(context.Background(), fx.alertID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	return refs[0]
}

func TestInboundAckStopsEscalationAndDoesNotLoop(t *testing.T) {
	fx := newPagingFixture(t)
	rec := fx.post(t, "incident.acknowledged", fx.secret)
	require.Equal(t, http.StatusOK, rec.Code)

	a, err := fx.st.GetAlert(context.Background(), fx.alertID)
	require.NoError(t, err)
	require.NotNil(t, a.AcknowledgedAt, "inbound ack goes through the store's acknowledgement path")
	assert.Equal(t, "pagerduty:Alice", a.AcknowledgedBy, "control characters are stripped from the provider-supplied actor")
	ref := fx.ref(t)
	assert.Equal(t, store.ExternalStateAcknowledged, ref.State)
	assert.Equal(t, "PINC1", ref.IncidentID, "the provider incident id is recorded in a column")
	assert.Empty(t, fx.calls, "an update that came from the provider is never sent back")

	// Even an explicit propagate from an inbound context is refused.
	fx.sync.Propagate(WithOrigin(context.Background(), "pagerduty"), fx.alertID, "acknowledge")
	assert.Empty(t, fx.calls)
}

func TestInboundRefusedWithoutValidSignature(t *testing.T) {
	fx := newPagingFixture(t)
	assert.Equal(t, http.StatusUnauthorized, fx.post(t, "incident.acknowledged", "wrong-secret").Code)
	assert.Equal(t, http.StatusUnauthorized, fx.post(t, "incident.acknowledged", "").Code)

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/hooks/pagerduty/%s/%d", workspace.Default, fx.chID), bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	fx.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no signature header")

	req = httptest.NewRequest(http.MethodPost, "/hooks/pagerduty/default/99999", bytes.NewBufferString(`{}`))
	rec = httptest.NewRecorder()
	fx.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "unknown channel is indistinguishable from a bad signature")

	a, err := fx.st.GetAlert(context.Background(), fx.alertID)
	require.NoError(t, err)
	assert.Nil(t, a.AcknowledgedAt, "an unauthenticated request cannot silence an alert")
}

func TestVerifyPagerDutySignatureEmptySecretNeverVerifies(t *testing.T) {
	mac := hmac.New(sha256.New, nil)
	mac.Write([]byte("x"))
	assert.False(t, VerifyPagerDutySignature("", []byte("x"), "v1="+hex.EncodeToString(mac.Sum(nil))))
}

func TestLocalAckPropagatesOnceToProvider(t *testing.T) {
	fx := newPagingFixture(t)
	require.NoError(t, fx.st.AcknowledgeAlert(context.Background(), fx.alertID, "bob"))
	fx.sync.Propagate(context.Background(), fx.alertID, "acknowledge")
	fx.sync.Propagate(context.Background(), fx.alertID, "acknowledge")
	require.Len(t, fx.calls, 1, "the provider is told once; the stored state suppresses repeats")
	assert.Equal(t, "acknowledge", fx.calls[0]["event_action"])
	assert.Equal(t, dedupKey(fx.alert), fx.calls[0]["dedup_key"])
	assert.Equal(t, store.ExternalStateAcknowledged, fx.ref(t).State)
}

func TestConflictResolutionIsMonotonicAndFirstAckWins(t *testing.T) {
	fx := newPagingFixture(t)
	ctx := context.Background()
	require.NoError(t, fx.st.AcknowledgeAlert(ctx, fx.alertID, "bob"))
	require.Equal(t, http.StatusOK, fx.post(t, "incident.resolved", fx.secret).Code)
	// A late acknowledgement event must not move a resolved incident back.
	require.Equal(t, http.StatusOK, fx.post(t, "incident.acknowledged", fx.secret).Code)
	assert.Equal(t, store.ExternalStateResolved, fx.ref(t).State)
	a, err := fx.st.GetAlert(ctx, fx.alertID)
	require.NoError(t, err)
	assert.Equal(t, "bob", a.AcknowledgedBy, "the first acknowledger is kept regardless of arrival order")
	// Local ack after provider resolve sends nothing: the provider is already ahead.
	fx.sync.Propagate(ctx, fx.alertID, "acknowledge")
	assert.Empty(t, fx.calls)
}

func TestUnreachableProviderKeepsLocalStateAndRecordsDivergence(t *testing.T) {
	fx := newPagingFixture(t)
	fx.pd.Close()
	ctx := context.Background()
	require.NoError(t, fx.st.AcknowledgeAlert(ctx, fx.alertID, "bob"))
	fx.sync.Propagate(ctx, fx.alertID, "acknowledge")
	ref := fx.ref(t)
	assert.Equal(t, store.ExternalStateOpen, ref.State, "the provider is not marked acknowledged")
	assert.Equal(t, "provider unreachable", ref.SyncError, "the divergence is visible, without any configuration")
	a, err := fx.st.GetAlert(ctx, fx.alertID)
	require.NoError(t, err)
	assert.NotNil(t, a.AcknowledgedAt, "local state stays authoritative")
}

func TestPagingRefsAreWorkspaceScoped(t *testing.T) {
	fx := newPagingFixture(t)
	other := workspace.With(context.Background(), workspace.ID("beta"))
	refs, err := fx.st.ListAlertExternalRefs(other, fx.alertID)
	require.NoError(t, err)
	assert.Empty(t, refs)
	_, err = fx.st.GetAlertExternalRefByDedupKey(other, fx.chID, ProviderPagerDuty, dedupKey(fx.alert))
	assert.ErrorIs(t, err, store.ErrNotFound)
}
