package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSignalValidConfig(t *testing.T) {
	n, err := NewSignal(json.RawMessage(`{"api_url": "http://signal-cli:8080", "number": "+15551234567", "recipients": ["+15559876543"]}`))
	require.NoError(t, err)
	require.NotNil(t, n)
	_, ok := n.(*Signal)
	assert.True(t, ok, "NewSignal must return a *Signal")
}

func TestNewSignalMalformedConfig(t *testing.T) {
	_, err := NewSignal(json.RawMessage(`{"api_url": }`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewSignalMissingAPIURL(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty api_url", `{"api_url": "", "number": "+15551234567", "recipients": ["+15559876543"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSignal(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "api_url is required")
		})
	}
}

func TestNewSignalMissingNumber(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"missing number", `{"api_url": "http://signal-cli:8080", "recipients": ["+15559876543"]}`},
		{"empty number", `{"api_url": "http://signal-cli:8080", "number": "", "recipients": ["+15559876543"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSignal(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "number is required")
		})
	}
}

func TestNewSignalMissingRecipients(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"missing recipients", `{"api_url": "http://signal-cli:8080", "number": "+15551234567"}`},
		{"empty recipients", `{"api_url": "http://signal-cli:8080", "number": "+15551234567", "recipients": []}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSignal(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "recipients must be a non-empty array")
		})
	}
}

func TestNewSignalInvalidTemplate(t *testing.T) {
	_, err := NewSignal(json.RawMessage(
		`{"api_url": "http://signal-cli:8080", "number": "+15551234567", "recipients": ["+15559876543"], "template": "{{.Unclosed"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid template")
}

func TestNewSignalAPIURLTrailingSlashNormalized(t *testing.T) {
	n, err := NewSignal(json.RawMessage(`{"api_url": "http://signal-cli:8080/", "number": "+15551234567", "recipients": ["+15559876543"]}`))
	require.NoError(t, err)
	s := n.(*Signal)
	assert.Equal(t, "http://signal-cli:8080", s.cfg.APIURL)
}

func TestSignalSendSuccess(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotURL         string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotURL = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := NewSignal(json.RawMessage(fmt.Sprintf(`{"api_url": %q, "number": "+15551234567", "recipients": ["+15559876543"]}`, srv.URL)))
	require.NoError(t, err)

	alert := Alert{ID: 7, MonitorName: "my-monitor", EventID: "ev-1"}
	require.NoError(t, n.Send(context.Background(), alert))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "/v2/send", gotURL)

	var payload struct {
		Message   string   `json:"message"`
		Number    string   `json:"number"`
		Recipients []string `json:"recipients"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "+15551234567", payload.Number)
	assert.Equal(t, []string{"+15559876543"}, payload.Recipients)
	assert.Contains(t, payload.Message, "my-monitor")
}

func TestSignalSendMultipleRecipients(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"api_url": %q, "number": "+15551234567", "recipients": ["+15559876543", "+15551112222", "group.123"]}`, srv.URL)
	n, err := NewSignal(json.RawMessage(cfg))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, EventID: "ev-42"}))

	var payload struct {
		Message    string   `json:"message"`
		Number     string   `json:"number"`
		Recipients []string `json:"recipients"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "+15551234567", payload.Number)
	assert.Equal(t, []string{"+15559876543", "+15551112222", "group.123"}, payload.Recipients)
}

func TestSignalSendCustomTemplate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`{"api_url": %q, "number": "+15551234567", "recipients": ["+15559876543"], "template": "alert {{.EventID}}"}`, srv.URL)
	n, err := NewSignal(json.RawMessage(cfg))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, EventID: "ev-42"}))

	var payload struct {
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "alert ev-42", payload.Message)
}

func TestSignalSendServerError(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"client error", http.StatusBadRequest},
		{"server error", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tt.status)
			}))
			defer srv.Close()

			n, err := NewSignal(json.RawMessage(fmt.Sprintf(`{"api_url": %q, "number": "+15551234567", "recipients": ["+15559876543"]}`, srv.URL)))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", tt.status))
			// Ensure no recipient numbers are in the error message
			assert.NotContains(t, err.Error(), "15559876543")
			assert.NotContains(t, err.Error(), "15551234567")
		})
	}
}

func TestSignalSendNetworkFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	n, err := NewSignal(json.RawMessage(fmt.Sprintf(`{"api_url": %q, "number": "+15551234567", "recipients": ["+15559876543"]}`, url)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	require.Error(t, err)
	// Network errors should be wrapped
	assert.Contains(t, strings.ToLower(err.Error()), "post")
}

func TestSignalSendHTTPOKButNon2xx(t *testing.T) {
	// Test that 3xx redirects are treated as errors (postJSON treats non-2xx as error)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	n, err := NewSignal(json.RawMessage(fmt.Sprintf(`{"api_url": %q, "number": "+15551234567", "recipients": ["+15559876543"]}`, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "301")
}