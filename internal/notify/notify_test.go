package notify

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureNotifier struct {
	config json.RawMessage
}

func (c *captureNotifier) Send(ctx context.Context, a Alert) error {
	return nil
}

func TestFactory_NewContext(t *testing.T) {
	f := DefaultFactory()
	f.Register("capture", func(cfg json.RawMessage) (Notifier, error) {
		return &captureNotifier{config: cfg}, nil
	})
	f.Register("error", func(cfg json.RawMessage) (Notifier, error) {
		return nil, errors.New("constructor error")
	})

	tests := []struct {
		name        string
		ctx         context.Context
		channelType string
		config      json.RawMessage
		wantErr     bool
		errContains string
	}{
		{
			name:        "happy path - known channel type with valid config",
			ctx:         context.Background(),
			channelType: "capture",
			config:      json.RawMessage(`{"key":"value"}`),
			wantErr:     false,
		},
		{
			name:        "happy path - empty config",
			ctx:         context.Background(),
			channelType: "capture",
			config:      json.RawMessage(`{}`),
			wantErr:     false,
		},
		{
			name:        "error path - unknown channel type",
			ctx:         context.Background(),
			channelType: "unknown_type",
			config:      json.RawMessage(`{}`),
			wantErr:     true,
			errContains: "unknown channel type",
		},
		{
			name:        "error path - constructor returns error",
			ctx:         context.Background(),
			channelType: "error",
			config:      json.RawMessage(`{}`),
			wantErr:     true,
			errContains: "constructor error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notifier, err := f.NewContext(tt.ctx, tt.channelType, tt.config)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				assert.Nil(t, notifier)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, notifier)
			}
		})
	}
}

func TestFactory_NewContext_WithSecrets(t *testing.T) {
	captured := struct {
		WebhookURL string `json:"webhook_url"`
	}{}
	f := DefaultFactory()
	f.Register("capture-secret", func(cfg json.RawMessage) (Notifier, error) {
		return nil, json.Unmarshal(cfg, &captured)
	})

	// Test without secrets resolver - reference passed as literal
	if _, err := f.NewContext(context.Background(), "capture-secret", json.RawMessage(`{"webhook_url":"${secret:env:TEST}"}`)); err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	if captured.WebhookURL != "${secret:env:TEST}" {
		t.Fatalf("reference not passed as literal: %q", captured.WebhookURL)
	}
}

func TestFactory_NewContext_CallsConstructorWithResolvedConfig(t *testing.T) {
	var receivedConfig json.RawMessage
	f := DefaultFactory()
	f.Register("config-check", func(cfg json.RawMessage) (Notifier, error) {
		receivedConfig = cfg
		return &captureNotifier{}, nil
	})

	config := json.RawMessage(`{"key":"value","number":42}`)
	_, err := f.NewContext(context.Background(), "config-check", config)
	require.NoError(t, err)

	var expected, actual map[string]any
	require.NoError(t, json.Unmarshal(config, &expected))
	require.NoError(t, json.Unmarshal(receivedConfig, &actual))
	assert.Equal(t, expected, actual)
}