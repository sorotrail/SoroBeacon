package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTwilio_Send(t *testing.T) {
	var requests []struct {
		authHeader  string
		contentType string
		formBody    url.Values
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		ct := r.Header.Get("Content-Type")
		bodyBytes, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(bodyBytes))

		requests = append(requests, struct {
			authHeader  string
			contentType string
			formBody    url.Values
		}{
			authHeader:  auth,
			contentType: ct,
			formBody:    vals,
		})

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"SM123"}`))
	}))
	defer srv.Close()

	cfgJSON, err := json.Marshal(map[string]any{
		"account_sid": "AC12345",
		"auth_token":  "secret_token",
		"from":        "+15005550006",
		"to":          []string{"+15551234567", "+15559876543"},
		"api_base":    srv.URL,
	})
	require.NoError(t, err)

	notifier, err := NewTwilio(cfgJSON)
	require.NoError(t, err)

	alert := Alert{
		ID:          1,
		MonitorID:   2,
		MonitorName: "Test Monitor",
		RuleID:      3,
		RuleType:    "event_emitted",
		EventID:     "evt-1",
		ContractID:  "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		EventName:   "transfer",
		Ledger:      12345,
		TxHash:      "abc123hash",
		CreatedAt:   time.Now(),
	}

	err = notifier.Send(context.Background(), alert)
	assert.NoError(t, err)

	assert.Len(t, requests, 2)

	expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("AC12345:secret_token"))
	for _, req := range requests {
		assert.Equal(t, expectedAuth, req.authHeader)
		assert.Equal(t, "application/x-www-form-urlencoded", req.contentType)
		assert.Equal(t, "+15005550006", req.formBody.Get("From"))
		assert.NotEmpty(t, req.formBody.Get("To"))
		assert.NotEmpty(t, req.formBody.Get("Body"))
	}
}

func TestTwilio_Truncation(t *testing.T) {
	var sentBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(bodyBytes))
		sentBody = vals.Get("Body")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfgJSON, err := json.Marshal(map[string]any{
		"account_sid": "AC12345",
		"auth_token":  "token",
		"from":        "+15005550006",
		"to":          []string{"+15551234567"},
		"api_base":    srv.URL,
	})
	require.NoError(t, err)

	notifier, err := NewTwilio(cfgJSON)
	require.NoError(t, err)

	alert := Alert{
		MonitorName: "Very Long Monitor Name That Exceeds Normal SMS Length Limits Easily and Should Trigger Truncation Logic To Fit Within The Limit",
		ContractID:  "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
		EventName:   "transfer_with_very_long_name",
	}

	err = notifier.Send(context.Background(), alert)
	assert.NoError(t, err)
	assert.LessOrEqual(t, len(sentBody), 160)
}
