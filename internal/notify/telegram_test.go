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
				// Simulating a dropped connection by sending incorrect Content-Length
				// which causes the client to get an unexpected EOF.
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
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
