package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI is an in-memory SoroBeacon channels API.
type fakeAPI struct {
	mu       sync.Mutex
	channels map[int64]map[string]any
	next     int64
	writes   int
	down     bool
	srv      *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{channels: map[int64]map[string]any{}, next: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		if r.Header.Get("Authorization") != "Bearer scoped-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == http.MethodGet:
			list := []map[string]any{}
			for id, c := range f.channels {
				list = append(list, map[string]any{"id": id, "name": c["name"], "type": c["type"], "enabled": c["enabled"], "min_severity": c["min_severity"]})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"channels": list, "next_cursor": ""})
		case r.Method == http.MethodPost:
			f.writes++
			id := f.next
			f.next++
			f.channels[id] = body
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		default:
			id, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
			if _, ok := f.channels[id]; !ok {
				http.NotFound(w, r)
				return
			}
			f.writes++
			if r.Method == http.MethodDelete {
				delete(f.channels, id)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			f.channels[id] = body
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client() *BeaconClient {
	return &BeaconClient{BaseURL: f.srv.URL, Token: "scoped-token"}
}

type fakeKube struct {
	secrets    map[string]map[string][]byte
	finalizers []string
	status     ChannelStatus
	statusSets int
}

func (k *fakeKube) ListChannels(context.Context) ([]Channel, error) { return nil, nil }
func (k *fakeKube) GetSecret(_ context.Context, _, name string) (map[string][]byte, error) {
	if s, ok := k.secrets[name]; ok {
		return s, nil
	}
	return nil, &StatusError{Code: 404}
}
func (k *fakeKube) SetFinalizers(_ context.Context, _, _ string, f []string) error {
	k.finalizers = f
	return nil
}
func (k *fakeKube) UpdateStatus(_ context.Context, _, _ string, s ChannelStatus) error {
	k.status = s
	k.statusSets++
	return nil
}

func slackCR(k *fakeKube) Channel {
	return Channel{
		Metadata: Meta{Name: "oncall", Namespace: "ops", Generation: 1, Finalizers: k.finalizers},
		Spec: ChannelSpec{Type: "slack", MinSeverity: "warning",
			SecretRefs: []SecretRef{{ConfigKey: "webhook_url", Name: "slack-hook", Key: "url"}}},
		Status: k.status,
	}
}

func setup(t *testing.T) (*Reconciler, *fakeKube, *fakeAPI) {
	k := &fakeKube{secrets: map[string]map[string][]byte{"slack-hook": {"url": []byte("https://hooks.slack.test/SECRET")}}}
	api := newFakeAPI(t)
	return &Reconciler{Kube: k, Beacon: api.client()}, k, api
}

func TestCreateAppliesWithSecretAndKeepsSecretOutOfStatus(t *testing.T) {
	r, k, api := setup(t)
	r.Reconcile(context.Background(), slackCR(k))
	assert.Equal(t, PhaseApplied, k.status.Phase)
	assert.Contains(t, k.finalizers, Finalizer, "the finalizer is set before anything is created")
	require.Len(t, api.channels, 1)
	cfg, _ := json.Marshal(api.channels[1]["config"])
	assert.Contains(t, string(cfg), "https://hooks.slack.test/SECRET", "the secret value reaches the API")
	st, _ := json.Marshal(k.status)
	assert.NotContains(t, string(st), "SECRET")
	assert.NotContains(t, string(st), "hooks.slack")
}

func TestReconcileIsIdempotent(t *testing.T) {
	r, k, api := setup(t)
	r.Reconcile(context.Background(), slackCR(k))
	r.Reconcile(context.Background(), slackCR(k)) // settles the message to in sync
	writes, sets := api.writes, k.statusSets
	r.Reconcile(context.Background(), slackCR(k))
	r.Reconcile(context.Background(), slackCR(k))
	assert.Equal(t, writes, api.writes, "repeated passes write nothing")
	assert.Equal(t, sets, k.statusSets, "and do not churn status")
	assert.Equal(t, "in sync", k.status.Message)
}

func TestDriftIsRestoredAndReported(t *testing.T) {
	r, k, api := setup(t)
	r.Reconcile(context.Background(), slackCR(k))
	api.channels[1]["enabled"] = false // someone disabled it through the API
	r.Reconcile(context.Background(), slackCR(k))
	assert.Equal(t, true, api.channels[1]["enabled"])
	assert.Contains(t, k.status.Message, "restored")
}

func TestSecretRotationIsReapplied(t *testing.T) {
	r, k, api := setup(t)
	r.Reconcile(context.Background(), slackCR(k))
	k.secrets["slack-hook"]["url"] = []byte("https://hooks.slack.test/ROTATED")
	r.Reconcile(context.Background(), slackCR(k))
	cfg, _ := json.Marshal(api.channels[1]["config"])
	assert.Contains(t, string(cfg), "ROTATED")
}

func TestRemoteDeletedIsRecreated(t *testing.T) {
	r, k, api := setup(t)
	r.Reconcile(context.Background(), slackCR(k))
	delete(api.channels, 1)
	r.Reconcile(context.Background(), slackCR(k))
	assert.Len(t, api.channels, 1)
	assert.EqualValues(t, 2, k.status.ChannelID)
}

func TestNameCollisionNeedsAdopt(t *testing.T) {
	r, k, api := setup(t)
	api.channels[5] = map[string]any{"name": "oncall", "type": "slack", "enabled": true, "min_severity": ""}
	r.Reconcile(context.Background(), slackCR(k))
	assert.Equal(t, PhaseError, k.status.Phase)
	assert.Contains(t, k.status.Message, "adopt")
	assert.Len(t, api.channels, 1, "the hand-made channel is untouched")

	cr := slackCR(k)
	cr.Spec.Adopt = true
	r.Reconcile(context.Background(), cr)
	assert.Equal(t, PhaseApplied, k.status.Phase)
	assert.EqualValues(t, 5, k.status.ChannelID)
}

func TestMissingSecretReportsErrorWithoutValues(t *testing.T) {
	r, k, api := setup(t)
	delete(k.secrets, "slack-hook")
	r.Reconcile(context.Background(), slackCR(k))
	assert.Equal(t, PhaseError, k.status.Phase)
	assert.Contains(t, k.status.Message, "slack-hook")
	assert.Empty(t, api.channels)
}

func TestUnreachableAPIIsReportedNotSilent(t *testing.T) {
	r, k, api := setup(t)
	api.down = true
	r.Reconcile(context.Background(), slackCR(k))
	assert.Equal(t, PhaseError, k.status.Phase)
	assert.Contains(t, k.status.Message, "will retry")
	api.down = false
	r.Reconcile(context.Background(), slackCR(k))
	assert.Equal(t, PhaseApplied, k.status.Phase, "it converges once the API is back")
}

func deleting(cr Channel) Channel {
	cr.Metadata.DeletionTimestamp = "2026-11-19T00:00:00Z"
	return cr
}

func TestDeleteOrphansByDefault(t *testing.T) {
	r, k, api := setup(t)
	r.Reconcile(context.Background(), slackCR(k))
	r.Reconcile(context.Background(), deleting(slackCR(k)))
	assert.Len(t, api.channels, 1, "removing the manifest does not remove the channel")
	assert.NotContains(t, k.finalizers, Finalizer)
}

func TestDeletePolicyDeleteRemovesOnlyTheChannel(t *testing.T) {
	r, k, api := setup(t)
	api.channels[9] = map[string]any{"name": "other", "type": "slack", "enabled": true}
	cr := slackCR(k)
	cr.Spec.DeletionPolicy = DeletionDelete
	r.Reconcile(context.Background(), cr)
	cr = slackCR(k)
	cr.Spec.DeletionPolicy = DeletionDelete
	r.Reconcile(context.Background(), deleting(cr))
	assert.Len(t, api.channels, 1)
	assert.Contains(t, api.channels, int64(9), "other channels are never touched")
	assert.NotContains(t, k.finalizers, Finalizer)
}

func TestDeleteBlockedWhileAPIUnreachable(t *testing.T) {
	r, k, api := setup(t)
	cr := slackCR(k)
	cr.Spec.DeletionPolicy = DeletionDelete
	r.Reconcile(context.Background(), cr)
	api.down = true
	cr = slackCR(k)
	cr.Spec.DeletionPolicy = DeletionDelete
	r.Reconcile(context.Background(), deleting(cr))
	assert.Contains(t, k.finalizers, Finalizer, "the finalizer holds the resource until the API confirms")
	assert.Equal(t, PhaseDeleting, k.status.Phase)
}

func TestScopedTokenIsUsed(t *testing.T) {
	r, k, api := setup(t)
	r.Beacon = &BeaconClient{BaseURL: api.srv.URL, Token: "wrong"}
	r.Reconcile(context.Background(), slackCR(k))
	assert.Equal(t, PhaseError, k.status.Phase)
	assert.Contains(t, k.status.Message, "will retry")
}
