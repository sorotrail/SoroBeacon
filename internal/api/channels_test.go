package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// channelsStore records the enabledOnly flag ListChannels was called with,
// and returns a fixed set of channels regardless of it — the filtering
// itself is the store's job (tested against Postgres); the handler's job
// is wiring the query param through correctly.
type channelsStore struct {
	store.Store
	gotEnabledOnly *bool
	gotType        string
}

func (c *channelsStore) ListChannelsPage(ctx context.Context, f store.ListFilter) ([]store.Channel, error) {
	c.gotEnabledOnly = &f.EnabledOnly
	c.gotType = f.Type
	return []store.Channel{{ID: 1, Name: "ops", Type: "webhook", Enabled: true}}, nil
}

// channelHealthCall records one RecordChannelHealth call, so a test can assert
// what a handler told the store about a channel's health.
type channelHealthCall struct {
	channelID int64
	update    store.ChannelHealthUpdate
}

// healthChannelStore serves one channel and records the health writes and
// channel updates handlers make against it.
type healthChannelStore struct {
	store.Store
	channel  store.Channel
	channels map[int64]*store.Channel
	getErr   error
	health   []channelHealthCall
	updated  []store.Channel
}

func (s *healthChannelStore) ListChannelsPage(context.Context, store.ListFilter) ([]store.Channel, error) {
	return []store.Channel{s.channel}, nil
}

func (s *healthChannelStore) GetChannel(_ context.Context, id int64) (*store.Channel, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if len(s.channels) > 0 {
		ch, ok := s.channels[id]
		if !ok {
			return nil, store.ErrNotFound
		}
		copy := *ch
		return &copy, nil
	}
	if s.channel.ID != 0 && s.channel.ID != id {
		return nil, store.ErrNotFound
	}
	ch := s.channel
	return &ch, nil
}

func (s *healthChannelStore) UpdateChannel(_ context.Context, c *store.Channel) error {
	s.updated = append(s.updated, *c)
	return nil
}

func (s *healthChannelStore) RecordChannelHealth(_ context.Context, channelID int64, u store.ChannelHealthUpdate) error {
	s.health = append(s.health, channelHealthCall{channelID: channelID, update: u})
	return nil
}

func (s *healthChannelStore) CreateAuditEntry(_ context.Context, _ *store.AuditEntry) error {
	return nil
}

// healthServer wires an API server whose "mock" channel type is backed by n.
func healthServer(t *testing.T, st store.Store, n notify.Notifier) *httptest.Server {
	t.Helper()
	return healthServerWithLogger(t, st, n, discardLogger())
}

// healthServerWithLogger wires an API server capturing logs into the provided logger.
func healthServerWithLogger(t *testing.T, st store.Store, n notify.Notifier, logger *slog.Logger) *httptest.Server {
	t.Helper()
	f := notify.DefaultFactory()
	f.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return n, nil })
	return httptest.NewServer(New(st, rules.NewRegistry(), f, &fakeRPC{}, logger).Routes())
}

// A channel that has stopped working has to say so in the listing: reading
// delivery attempts one alert at a time was the only way to find out before.
func TestListChannels_SurfacesHealth(t *testing.T) {
	failedAt := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	st := &healthChannelStore{channel: store.Channel{
		ID: 1, Name: "ops", Type: "webhook", Enabled: false,
		ConsecutiveFailures:          7,
		ConsecutivePermanentFailures: 7,
		LastError:                    "status 401: Unauthorized",
		LastErrorAt:                  &failedAt,
		DisabledAt:                   &failedAt,
	}}

	code, body := getJSON(t, st, "/channels")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	list, _ := body["channels"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d channels, want 1 (body=%v)", len(list), body)
	}
	ch, _ := list[0].(map[string]any)
	if ch["consecutive_failures"] != float64(7) {
		t.Fatalf("consecutive_failures = %v, want 7", ch["consecutive_failures"])
	}
	if ch["last_error"] != "status 401: Unauthorized" {
		t.Fatalf("last_error = %v", ch["last_error"])
	}
	if ch["disabled_at"] == nil {
		t.Fatalf("an auto-disabled channel must report disabled_at, got %v", ch)
	}
}

// The "send test" button is how an operator checks a channel they have just
// fixed, so its outcome has to reach the health counters — including on the
// way back up, which is what stops a repaired channel being reported as
// broken for the rest of the day.
func TestTestChannel_RecordsHealth(t *testing.T) {
	t.Run("a successful send clears the failure state", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
		srv := healthServer(t, st, &scriptedNotifier{})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}

		if len(st.health) != 1 {
			t.Fatalf("got %d health updates, want 1", len(st.health))
		}
		got := st.health[0]
		if got.channelID != 4 {
			t.Fatalf("channel_id = %d, want 4", got.channelID)
		}
		if !got.update.Success {
			t.Fatalf("a delivered test must be recorded as a success: %+v", got.update)
		}
	})

	t.Run("a failing send records why, without a disable threshold", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
		srv := healthServer(t, st, &scriptedNotifier{err: &notify.HTTPStatusError{StatusCode: 401, Body: "Unauthorized"}})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", res.StatusCode)
		}

		if len(st.health) != 1 {
			t.Fatalf("got %d health updates, want 1", len(st.health))
		}
		got := st.health[0].update
		if got.Success {
			t.Fatal("a failed send must not be recorded as a success")
		}
		if !got.Permanent {
			t.Fatal("a 401 should count as permanent")
		}
		if !strings.Contains(got.Error, "401") {
			t.Fatalf("error = %q, want it to name the status", got.Error)
		}
		if got.DisableAfter != 0 {
			t.Fatalf("a diagnostic click must never carry a disable threshold, got %d", got.DisableAfter)
		}
	})
}

// Re-enabling is the one way back from auto-disable: it goes through the same
// channel update the API already had, and the store clears the counters that
// parked the channel as part of that write.
func TestUpdateChannel_ReenableGoesThroughUpdate(t *testing.T) {
	st := &healthChannelStore{channel: store.Channel{
		ID: 6, Name: "ops", Type: "mock", Config: json.RawMessage(`{}`), Enabled: false,
		ConsecutiveFailures: 3, ConsecutivePermanentFailures: 3,
	}}
	srv := healthServer(t, st, &scriptedNotifier{})
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/channels/6", strings.NewReader(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(st.updated) != 1 {
		t.Fatalf("got %d channel updates, want 1", len(st.updated))
	}
	if !st.updated[0].Enabled {
		t.Fatal("PATCH enabled=true must reach the store with the channel on")
	}
}

// A rename must not quietly wipe the evidence of a channel that is still
// failing, so nothing but an explicit re-enable clears the counters.
func TestUpdateChannel_RenameLeavesHealthAlone(t *testing.T) {
	st := &healthChannelStore{channel: store.Channel{
		ID: 6, Name: "ops", Type: "mock", Config: json.RawMessage(`{}`), Enabled: true,
		ConsecutiveFailures: 3, ConsecutivePermanentFailures: 3,
	}}
	srv := healthServer(t, st, &scriptedNotifier{})
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/channels/6", strings.NewReader(`{"name":"ops-renamed"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(st.health) != 0 {
		t.Fatalf("a rename wrote %d health updates, want 0", len(st.health))
	}
}

// The health update must never carry channel config: the notifiers redact
// URLs and tokens before building an error message.
func TestChannelHealthNeverRecordsSecrets(t *testing.T) {
	st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
	srv := healthServer(t, st, &scriptedNotifier{err: errors.New("status 403: forbidden")})
	defer srv.Close()

	res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if len(st.health) != 1 {
		t.Fatalf("got %d health updates, want 1", len(st.health))
	}
	if got := st.health[0].update.Error; strings.Contains(got, "webhook") || strings.Contains(got, "token") {
		t.Fatalf("health recorded something that reads like config: %q", got)
	}
}

func TestListChannels_EnabledParamWiring(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"no param", "", false},
		{"enabled=true", "?enabled=true", true},
		{"enabled=false leaves default behavior unchanged", "?enabled=false", false},
		{"enabled=garbage leaves default behavior unchanged", "?enabled=garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := &channelsStore{}
			srv := httptest.NewServer(newProbeServer(cs, &fakeRPC{}))
			defer srv.Close()

			res, err := http.Get(srv.URL + "/channels" + tt.query)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET /channels%s = %d, want 200", tt.query, res.StatusCode)
			}
			if cs.gotEnabledOnly == nil {
				t.Fatal("ListChannels was not called")
			}
			if *cs.gotEnabledOnly != tt.want {
				t.Fatalf("enabledOnly = %v, want %v", *cs.gotEnabledOnly, tt.want)
			}

			var body map[string]any
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			list, _ := body["channels"].([]any)
			if len(list) != 1 {
				t.Fatalf("got %d channels, want 1 (body=%v)", len(list), body)
			}
		})
	}
}

// TestListChannels_TypeFilter covers the ?type= filter. The filter itself is
// applied in SQL; what the handler owns is validating the value and passing
// it through, so an unknown type is a 400 rather than an empty list that
// reads like "no channels configured".
func TestListChannels_TypeFilter(t *testing.T) {
	t.Run("passes a known type through", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels?type=webhook")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if cs.gotType != "webhook" {
			t.Fatalf("type = %q, want %q", cs.gotType, "webhook")
		}
	})

	t.Run("rejects an unknown type", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels?type=carrier-pigeon")
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})

	t.Run("no type means no filter", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if cs.gotType != "" {
			t.Fatalf("type = %q, want empty", cs.gotType)
		}
	})
}

// assertNoSecretLeak verifies that a secret string does not appear in the
// serialized HTTP response body or in the captured slog output.
func assertNoSecretLeak(t *testing.T, secret string, body []byte, logBuf *bytes.Buffer) {
	t.Helper()
	if bytes.Contains(body, []byte(secret)) {
		t.Fatalf("response body leaked secret %q: %s", secret, string(body))
	}
	if logBuf != nil && strings.Contains(logBuf.String(), secret) {
		t.Fatalf("captured log output leaked secret %q: %s", secret, logBuf.String())
	}
}

// TestGetChannel covers the single-channel read endpoint (GET /channels/{id}).
//
// Credentials (webhook URLs, bot tokens, SMTP passwords) reside inside a channel's
// Config field. In accordance with the repository's security requirements, channel
// config is tagged with `json:"-"` and must never be logged, returned, or echoed.
func TestGetChannel(t *testing.T) {
	t.Run("returns channel metadata and omits secret config", func(t *testing.T) {
		fakeToken := "test-bot-token-xyz-secret"
		fakeURL := "https://hooks.slack.com/services/T00/B00/SECRETXYZ"
		cfgJSON := fmt.Sprintf(`{"bot_token":%q,"webhook_url":%q}`, fakeToken, fakeURL)

		st := &healthChannelStore{channel: store.Channel{
			ID:                  10,
			Name:                "ops-alerts",
			Type:                "mock",
			Config:              json.RawMessage(cfgJSON),
			Enabled:             true,
			MinSeverity:         store.SeverityCritical,
			DigestMode:          "window",
			DigestWindowSeconds: 300,
			Timeout:             5 * time.Second,
			ConsecutiveFailures: 2,
		}}

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
		srv := healthServerWithLogger(t, st, &scriptedNotifier{}, logger)
		defer srv.Close()

		res, err := http.Get(srv.URL + "/channels/10")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}

		rawBody, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}

		// The response body and logs must not leak the secret tokens or URLs.
		assertNoSecretLeak(t, fakeToken, rawBody, &logBuf)
		assertNoSecretLeak(t, fakeURL, rawBody, &logBuf)

		// Unmarshal into generic map to verify "config" key is completely omitted (json:"-").
		var rawMap map[string]any
		if err := json.Unmarshal(rawBody, &rawMap); err != nil {
			t.Fatalf("unmarshal response body: %v", err)
		}
		if _, exists := rawMap["config"]; exists {
			t.Fatalf("config field must be omitted from channel JSON serialization, got: %v", rawMap["config"])
		}

		// Verify metadata fields match
		if rawMap["id"] != float64(10) {
			t.Errorf("id = %v, want 10", rawMap["id"])
		}
		if rawMap["name"] != "ops-alerts" {
			t.Errorf("name = %v, want 'ops-alerts'", rawMap["name"])
		}
		if rawMap["type"] != "mock" {
			t.Errorf("type = %v, want 'mock'", rawMap["type"])
		}
		if rawMap["enabled"] != true {
			t.Errorf("enabled = %v, want true", rawMap["enabled"])
		}
		if rawMap["min_severity"] != "critical" {
			t.Errorf("min_severity = %v, want 'critical'", rawMap["min_severity"])
		}
		if rawMap["digest_mode"] != "window" {
			t.Errorf("digest_mode = %v, want 'window'", rawMap["digest_mode"])
		}
	})

	t.Run("missing channel returns 404", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 10}}
		srv := healthServer(t, st, &scriptedNotifier{})
		defer srv.Close()

		res, err := http.Get(srv.URL + "/channels/999")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", res.StatusCode)
		}
		var errBody map[string]string
		if err := json.NewDecoder(res.Body).Decode(&errBody); err != nil {
			t.Fatal(err)
		}
		if errBody["error"] != "not found" {
			t.Errorf("error = %q, want 'not found'", errBody["error"])
		}
	})

	t.Run("invalid id format returns 400", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 10}}
		srv := healthServer(t, st, &scriptedNotifier{})
		defer srv.Close()

		res, err := http.Get(srv.URL + "/channels/not-a-number")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("store error surfaces as 500 without leaking raw error text", func(t *testing.T) {
		secretDBErr := "pq: password authentication failed for user /var/secrets/postgres.key"
		st := &healthChannelStore{
			channel: store.Channel{ID: 10},
			getErr:  errors.New(secretDBErr),
		}
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
		srv := healthServerWithLogger(t, st, &scriptedNotifier{}, logger)
		defer srv.Close()

		res, err := http.Get(srv.URL + "/channels/10")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", res.StatusCode)
		}
		rawBody, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(rawBody, []byte(secretDBErr)) {
			t.Fatalf("500 response body leaked raw store error: %s", string(rawBody))
		}
		var errBody map[string]string
		if err := json.Unmarshal(rawBody, &errBody); err != nil {
			t.Fatal(err)
		}
		if errBody["error"] != "internal error" {
			t.Errorf("error = %q, want 'internal error'", errBody["error"])
		}
	})

	t.Run("cross-workspace channel read returns 404", func(t *testing.T) {
		st := newTenantStore(t)
		acmeCh := &store.Channel{
			Name:    "acme-chan",
			Type:    "webhook",
			Config:  json.RawMessage(`{"url":"https://acme.example.com"}`),
			Enabled: true,
		}
		if err := st.CreateChannel(workspace.With(context.Background(), "acme"), acmeCh); err != nil {
			t.Fatalf("create acme channel: %v", err)
		}

		h := authServerOn(st, twoWorkspaces())

		// Acme reads its own channel successfully
		resAcme := get(t, h, "/channels/"+strconv.FormatInt(acmeCh.ID, 10), bearer(acmeToken))
		defer resAcme.Body.Close()
		if resAcme.StatusCode != http.StatusOK {
			t.Fatalf("acme reading own channel: status = %d, want 200", resAcme.StatusCode)
		}

		// Beta reading acme's channel gets 404 (same as non-existent)
		resBeta := get(t, h, "/channels/"+strconv.FormatInt(acmeCh.ID, 10), bearer(betaToken))
		defer resBeta.Body.Close()
		if resBeta.StatusCode != http.StatusNotFound {
			t.Fatalf("beta reading acme channel: status = %d, want 404", resBeta.StatusCode)
		}

		// Reading non-existent ID also gets 404
		resMissing := get(t, h, "/channels/999999", bearer(acmeToken))
		defer resMissing.Body.Close()
		if resMissing.StatusCode != http.StatusNotFound {
			t.Fatalf("missing channel: status = %d, want 404", resMissing.StatusCode)
		}
	})

	t.Run("scoped token enforces channels:read", func(t *testing.T) {
		h, _, st := newTokenServer(t, nil)
		ch := &store.Channel{
			Name:    "scoped-ch",
			Type:    "webhook",
			Config:  json.RawMessage(`{}`),
			Enabled: true,
		}
		if err := st.CreateChannel(workspace.With(context.Background(), "acme"), ch); err != nil {
			t.Fatalf("create channel: %v", err)
		}

		path := "/channels/" + strconv.FormatInt(ch.ID, 10)

		// Unauthenticated -> 401
		resUnauth := callJSON(t, h, http.MethodGet, path, "", "")
		if resUnauth.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated read = %d, want 401", resUnauth.Code)
		}

		// Token with monitors:read -> 403
		monitorsTok := mint(t, h, `{"name":"mon","scopes":["monitors:read"]}`)
		resMon := callJSON(t, h, http.MethodGet, path, monitorsTok, "")
		if resMon.Code != http.StatusForbidden {
			t.Errorf("monitors:read token = %d, want 403", resMon.Code)
		}

		// Token with channels:read -> 200
		channelsTok := mint(t, h, `{"name":"chan","scopes":["channels:read"]}`)
		resChan := callJSON(t, h, http.MethodGet, path, channelsTok, "")
		if resChan.Code != http.StatusOK {
			t.Errorf("channels:read token = %d, want 200", resChan.Code)
		}

		// Token with channels:write (which implies channels:read) -> 200
		writeTok := mint(t, h, `{"name":"chan-w","scopes":["channels:write"]}`)
		resWrite := callJSON(t, h, http.MethodGet, path, writeTok, "")
		if resWrite.Code != http.StatusOK {
			t.Errorf("channels:write token = %d, want 200", resWrite.Code)
		}
	})
}

// TestTestChannel_Extended covers edge cases, security invariants, error
// sanitization, rate limiting, and scope isolation for POST /channels/{id}/test.
func TestTestChannel_Extended(t *testing.T) {
	t.Run("sanitizes error quoting token secret and records in store", func(t *testing.T) {
		fakeSecret := "test-secret-token-xyz-12345678"
		fakeHook := "https://hooks.slack.com/services/T00/B00/SECRET123"
		rawErr := &url.Error{
			Op:  "Post",
			URL: fakeHook,
			Err: &notify.HTTPStatusError{
				StatusCode: 401,
				Body:       "unauthorized token: " + fakeSecret,
			},
		}

		st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
		srv := healthServerWithLogger(t, st, &scriptedNotifier{err: rawErr}, logger)
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", res.StatusCode)
		}

		if len(st.health) != 1 {
			t.Fatalf("got %d health updates, want 1", len(st.health))
		}
		update := st.health[0].update
		if update.Success {
			t.Fatal("failed test must not record success")
		}
		if !update.Permanent {
			t.Fatal("401 unauthorized must be classified as permanent")
		}

		// In-store recorded last_error MUST NOT contain the fake secret or hook URL
		assertNoSecretLeak(t, fakeSecret, []byte(update.Error), nil)
		assertNoSecretLeak(t, "SECRET123", []byte(update.Error), nil)

		// Logs must not leak either
		if strings.Contains(logBuf.String(), fakeSecret) {
			t.Fatalf("captured logs leaked secret: %s", logBuf.String())
		}
	})

	t.Run("records transient failure correctly", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
		srv := healthServer(t, st, &scriptedNotifier{err: &notify.HTTPStatusError{StatusCode: 503, Body: "Service Unavailable"}})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", res.StatusCode)
		}

		if len(st.health) != 1 {
			t.Fatalf("got %d health updates, want 1", len(st.health))
		}
		update := st.health[0].update
		if update.Success {
			t.Fatal("failed test must not record success")
		}
		if update.Permanent {
			t.Fatal("503 service unavailable must be classified as transient (Permanent=false)")
		}
	})

	t.Run("test send on disabled channel delivers test alert to allow verification", func(t *testing.T) {
		// Operators use "test send" to verify a disabled channel is working again before re-enabling it.
		notifier := &scriptedNotifier{}
		st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: false}}
		srv := healthServer(t, st, notifier)
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}

		if notifier.n != 1 {
			t.Fatalf("delivered alerts count = %d, want 1", notifier.n)
		}
		if len(st.health) != 1 || !st.health[0].update.Success {
			t.Fatalf("health update = %+v, want success=true", st.health)
		}
	})

	t.Run("missing channel returns 404", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 4}}
		srv := healthServer(t, st, &scriptedNotifier{})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/999/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", res.StatusCode)
		}
	})

	t.Run("invalid id format returns 400", func(t *testing.T) {
		st := &healthChannelStore{channel: store.Channel{ID: 4}}
		srv := healthServer(t, st, &scriptedNotifier{})
		defer srv.Close()

		res, err := http.Post(srv.URL+"/channels/abc/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("cross-workspace channel test returns 404", func(t *testing.T) {
		st := newTenantStore(t)
		acmeCh := &store.Channel{
			Name:    "acme-hook",
			Type:    "mock",
			Config:  json.RawMessage(`{}`),
			Enabled: true,
		}
		if err := st.CreateChannel(workspace.With(context.Background(), "acme"), acmeCh); err != nil {
			t.Fatalf("create acme channel: %v", err)
		}

		f := notify.DefaultFactory()
		f.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return &scriptedNotifier{}, nil })
		h := New(st, rules.NewRegistry(), f, &fakeRPC{}, discardLogger()).WithAuth(twoWorkspaces()).Routes()

		// Beta cannot test acme's channel (404)
		resBeta := callJSON(t, h, http.MethodPost, "/channels/"+strconv.FormatInt(acmeCh.ID, 10)+"/test", betaToken, "")
		if resBeta.Code != http.StatusNotFound {
			t.Fatalf("beta testing acme channel: status = %d, want 404", resBeta.Code)
		}

		// Acme can test its own channel (200)
		resAcme := callJSON(t, h, http.MethodPost, "/channels/"+strconv.FormatInt(acmeCh.ID, 10)+"/test", acmeToken, "")
		if resAcme.Code != http.StatusOK {
			t.Fatalf("acme testing own channel: status = %d, want 200", resAcme.Code)
		}
	})

	t.Run("scoped token enforces channels:write", func(t *testing.T) {
		st := newTenantStore(t)
		ch := &store.Channel{
			Name:    "scoped-hook",
			Type:    "mock",
			Config:  json.RawMessage(`{}`),
			Enabled: true,
		}
		if err := st.CreateChannel(workspace.With(context.Background(), "acme"), ch); err != nil {
			t.Fatalf("create channel: %v", err)
		}

		f := notify.DefaultFactory()
		f.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return &scriptedNotifier{}, nil })
		mgr := auth.NewManager(st, discardLogger())
		h := New(st, rules.NewRegistry(), f, &fakeRPC{}, discardLogger()).
			WithAuth(twoWorkspaces()).WithTokens(mgr).Routes()

		path := "/channels/" + strconv.FormatInt(ch.ID, 10) + "/test"

		// Token with channels:read only cannot test (403)
		readTok := mint(t, h, `{"name":"r-only","scopes":["channels:read"]}`)
		resRead := callJSON(t, h, http.MethodPost, path, readTok, "")
		if resRead.Code != http.StatusForbidden {
			t.Fatalf("channels:read token testing channel: status = %d, want 403", resRead.Code)
		}

		// Token with channels:write can test (200)
		writeTok := mint(t, h, `{"name":"w-only","scopes":["channels:write"]}`)
		resWrite := callJSON(t, h, http.MethodPost, path, writeTok, "")
		if resWrite.Code != http.StatusOK {
			t.Fatalf("channels:write token testing channel: status = %d, want 200 (%s)", resWrite.Code, resWrite.Body)
		}
	})

	t.Run("bounded by rate limiter against abuse", func(t *testing.T) {
		// POST /channels/{id}/test triggers an outbound notification, so it must be
		// bounded by RateLimitMiddleware to protect against outbound spam / abuse vectors.
		st := &healthChannelStore{channel: store.Channel{ID: 4, Name: "ops", Type: "mock", Enabled: true}}
		f := notify.DefaultFactory()
		f.Register("mock", func(json.RawMessage) (notify.Notifier, error) { return &scriptedNotifier{}, nil })
		srv := httptest.NewServer(
			New(st, rules.NewRegistry(), f, &fakeRPC{}, discardLogger()).
				WithRateLimit(RateLimitConfig{RPS: 1, Burst: 1}).
				Routes(),
		)
		defer srv.Close()

		// First request consumed the token
		res1, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		res1.Body.Close()
		if res1.StatusCode != http.StatusOK {
			t.Fatalf("first test request status = %d, want 200", res1.StatusCode)
		}

		// Immediate second request exceeds rate limit (429)
		res2, err := http.Post(srv.URL+"/channels/4/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		res2.Body.Close()
		if res2.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second test request status = %d, want 429", res2.StatusCode)
		}
	})
}
