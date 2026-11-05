package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// Coverage for the dead-letter dashboard page (GET /dead-letters) and redrive
// actions (POST /dead-letters/{id}/redrive, #521).

func TestDeadLettersPageStates(t *testing.T) {
	t.Run("empty queue renders the empty state", func(t *testing.T) {
		_, h, sess := newWriteDashboard(t, scriptedNotifier{})

		req := httptest.NewRequest(http.MethodGet, "/dead-letters", nil)
		req.AddCookie(sess)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		wantStatus(t, rec, http.StatusOK)

		body := rec.Body.String()
		assert.Contains(t, body, "No dead-lettered deliveries")
		assert.NotContains(t, body, "Backlog by Group")
	})

	t.Run("renders several letters, orphaned channel/alert IDs, filters, and never leaks channel config", func(t *testing.T) {
		st, h, sess := newWriteDashboard(t, scriptedNotifier{})
		const webhookSecret = "whsec_live_987654321_never_render"
		const botTokenSecret = "xoxb-secret-slack-bot-token"

		st.channels[1] = store.Channel{
			ID:     1,
			Name:   "pager-webhook",
			Type:   "webhook",
			Config: json.RawMessage(`{"url":"https://hooks.example.com/services/` + webhookSecret + `"}`),
		}
		st.channels[2] = store.Channel{
			ID:     2,
			Name:   "ops-slack",
			Type:   "slack",
			Config: json.RawMessage(`{"bot_token":"` + botTokenSecret + `"}`),
		}

		// Letter 1: active channel 1 and existing alert 10.
		st.alerts[10] = store.Alert{ID: 10, MonitorID: 1, RuleID: 1, EventID: "ev-10"}
		st.deadLetters[1] = store.DeadLetter{
			ID: 1, AlertID: 10, ChannelID: 1,
			Cause: store.DeadLetterCauseServerError, LastError: "status 502",
			AttemptCount: 4, LastStatus: 502,
			CreatedAt: time.Now().Add(-30 * time.Minute),
		}
		// Letter 2: alert 999 was since deleted, channel 2 still exists.
		st.deadLetters[2] = store.DeadLetter{
			ID: 2, AlertID: 999, ChannelID: 2,
			Cause: store.DeadLetterCauseRateLimit, LastError: "status 429",
			AttemptCount: 5, LastStatus: 429,
			CreatedAt: time.Now().Add(-3 * time.Hour),
		}
		// Letter 3: channel 888 was since deleted (orphaned channel fallback to numeric ID).
		st.deadLetters[3] = store.DeadLetter{
			ID: 3, AlertID: 10, ChannelID: 888,
			LastError: "connection refused", AttemptCount: 3, LastStatus: 0,
			CreatedAt: time.Now().Add(-48 * time.Hour),
		}

		req := httptest.NewRequest(http.MethodGet, "/dead-letters?channel_id=1&cause=server_error&older_than=1h", nil)
		req.AddCookie(sess)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		wantStatus(t, rec, http.StatusOK)

		body := rec.Body.String()
		assert.Contains(t, body, "pager-webhook")
		assert.Contains(t, body, "/alerts/10")
		assert.Contains(t, body, "server_error")
		assert.NotContains(t, body, webhookSecret, "rendered page must never contain webhook secret")
		assert.NotContains(t, body, botTokenSecret, "rendered page must never contain bot token")

		// Unfiltered view renders all three letters including deleted channel 888 and deleted alert 999.
		reqAll := httptest.NewRequest(http.MethodGet, "/dead-letters", nil)
		reqAll.AddCookie(sess)
		recAll := httptest.NewRecorder()
		h.ServeHTTP(recAll, reqAll)
		wantStatus(t, recAll, http.StatusOK)

		bodyAll := recAll.Body.String()
		assert.Contains(t, bodyAll, "pager-webhook")
		assert.Contains(t, bodyAll, "ops-slack")
		assert.Contains(t, bodyAll, ">888<", "deleted channel falls back to its numeric ID")
		assert.Contains(t, bodyAll, "/alerts/999", "letter whose alert was deleted still renders its alert ID")
		assert.NotContains(t, bodyAll, webhookSecret)
		assert.NotContains(t, bodyAll, botTokenSecret)
	})
}

func TestDeadLetterRedriveWithHTTPTestWebhookAndNoErrorLeak(t *testing.T) {
	var webhookStatus atomic.Int32
	webhookStatus.Store(http.StatusOK)
	var deliveries atomic.Int32

	const sensitiveUpstreamError = "internal-upstream-token-xyz-999-secret-failure"
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveries.Add(1)
		code := int(webhookStatus.Load())
		w.WriteHeader(code)
		if code >= 400 {
			_, _ = w.Write([]byte(sensitiveUpstreamError))
		}
	}))
	defer webhookSrv.Close()

	st := newWriteStore()
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	s.WithAuth(auth.New([]string{testToken}, 0))
	h := s.Routes()
	sess := sessionCookieOf(t, signIn(t, h, testToken, "/"))
	require.NotNil(t, sess)

	st.alerts[1] = store.Alert{
		ID: 1, MonitorID: 1, RuleID: 1, EventID: "ev-redrive-1",
		Severity: store.SeverityCritical, Payload: json.RawMessage(`{"contract":"C1"}`),
	}
	st.channels[1] = store.Channel{
		ID:      1,
		Name:    "httptest-webhook",
		Type:    "webhook",
		Enabled: true,
		Config:  json.RawMessage(`{"url":` + strconv.Quote(webhookSrv.URL) + `,"secret":"whsec-signing-secret-123"}`),
	}

	t.Run("failed delivery keeps the letter and renders failure page without notifier error text", func(t *testing.T) {
		webhookStatus.Store(http.StatusInternalServerError)
		st.deadLetters[1] = store.DeadLetter{
			ID: 1, AlertID: 1, ChannelID: 1,
			LastError: "status 500", AttemptCount: 3, LastStatus: 500,
		}

		rec := postWithSession(t, h, sess, "/dead-letters/1/redrive", nil)
		wantStatus(t, rec, http.StatusBadGateway)

		body := rec.Body.String()
		assert.Contains(t, body, "Redrive failed")
		assert.Contains(t, body, "delivery could not be redriven")
		assert.NotContains(t, body, sensitiveUpstreamError, "error page must not echo the notifier's error text")
		assert.NotContains(t, body, webhookSrv.URL, "error page must not echo the channel webhook URL")

		_, stillQueued := st.deadLetters[1]
		assert.True(t, stillQueued, "failed redrive must keep the dead letter in the queue")
	})

	t.Run("successful redrive over httptest endpoint removes the letter and redirects", func(t *testing.T) {
		webhookStatus.Store(http.StatusOK)
		beforeSends := deliveries.Load()

		rec := postWithSession(t, h, sess, "/dead-letters/1/redrive", nil)
		wantRedirect(t, rec, "/dead-letters")

		assert.Greater(t, deliveries.Load(), beforeSends, "webhook server should receive the redriven alert")
		_, stillQueued := st.deadLetters[1]
		assert.False(t, stillQueued, "successful redrive must remove the dead letter")
	})

	t.Run("unauthenticated or forged session cookie is rejected before redrive runs", func(t *testing.T) {
		st.deadLetters[5] = store.DeadLetter{ID: 5, AlertID: 1, ChannelID: 1}
		forged := &http.Cookie{Name: sess.Name, Value: "forged-csrf-or-session-token"}

		rec := postWithSession(t, h, forged, "/dead-letters/5/redrive", url.Values{})
		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), loginPath))
		_, stillQueued := st.deadLetters[5]
		assert.True(t, stillQueued, "rejected request must not redrive or delete the dead letter")
	})
}
