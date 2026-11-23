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
)

func TestNewTelegram(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{
			name:    "valid config",
			config:  `{"bot_token": "123:abc", "chat_id": "-100123456"}`,
			wantErr: false,
		},
		{
			name:    "malformed config",
			config:  `{bot_token: 123}`,
			wantErr: true,
		},
		{
			name:    "missing bot token",
			config:  `{"chat_id": "-100123456"}`,
			wantErr: true,
		},
		{
			name:    "missing chat id",
			config:  `{"bot_token": "123:abc"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTelegram(json.RawMessage(tt.config))
			if (err != nil) != tt.wantErr {
				t.Errorf("NewTelegram() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestTelegram_Send(t *testing.T) {
	alert := Alert{
		MonitorName: "Test Monitor",
	}

	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			},
			wantErr: false,
		},
		{
			name: "api error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request"}`))
			},
			wantErr: true,
		},
		{
			name: "dropped connection",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// Hijack and close without writing a response, so the client
				// sees the connection go away mid-request. An over-long
				// Content-Length would not do it: on a 2xx the sender only
				// inspects the status code and never reads the body, so a
				// truncated body is silently fine.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("test server does not support hijacking")
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = conn.Close()
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body []byte

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				body, err = io.ReadAll(r.Body)
				if err != nil {
					return
				}
				tt.handler(w, r)
			}))
			defer ts.Close()

			config := fmt.Sprintf(`{"bot_token": "secret_token", "chat_id": "-100123456", "api_base": "%s"}`, ts.URL)
			notifier, err := NewTelegram(json.RawMessage(config))
			if err != nil {
				t.Fatalf("failed to create notifier: %v", err)
			}

			err = notifier.Send(context.Background(), alert)
			if (err != nil) != tt.wantErr {
				t.Errorf("Send() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil {
				if strings.Contains(err.Error(), "secret_token") {
					t.Errorf("Send() error leaked bot token in error message")
				}
			}

			if tt.name != "dropped connection" {
				var payload map[string]interface{}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("failed to unmarshal request body: %v", err)
				}

				if chatID, ok := payload["chat_id"].(string); !ok || chatID != "-100123456" {
					t.Errorf("expected chat_id '-100123456', got %v", chatID)
				}
			}
		})
	}
}

func TestTelegramCheckUsesReadOnlyGetMe(t *testing.T) {
	called := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = r.Method + " " + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1}}`))
	}))
	defer ts.Close()

	notifier, err := NewTelegram(json.RawMessage(fmt.Sprintf(
		`{"bot_token":"secret_token","chat_id":"123","api_base":%q}`, ts.URL,
	)))
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.(PreflightChecker).Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if called != "GET /botsecret_token/getMe" {
		t.Fatalf("request = %q, want GET getMe endpoint", called)
	}
}
