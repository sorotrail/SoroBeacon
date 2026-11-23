package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSNSClient implements SNSAPI for testing.
type mockSNSClient struct {
	publishFunc func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

func (m *mockSNSClient) Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
	if m.publishFunc != nil {
		return m.publishFunc(ctx, params, optFns...)
	}
	return &sns.PublishOutput{MessageId: aws.String("test-message-id")}, nil
}

func TestNewSNSValidConfig(t *testing.T) {
	mockClient := &mockSNSClient{}
	tests := []struct {
		name   string
		config string
	}{
		{"with static credentials", `{"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon", "region": "us-east-1", "access_key_id": "AKIA...", "secret_access_key": "secret"}`},
		{"without static credentials (uses default chain)", `{"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon", "region": "us-east-1"}`},
		{"different region", `{"topic_arn": "arn:aws:sns:eu-west-1:123456789012:sorobeacon", "region": "eu-west-1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewSNSWithClient(json.RawMessage(tt.config), mockClient)
			require.NoError(t, err)
			require.NotNil(t, n)
			_, ok := n.(*SNS)
			assert.True(t, ok, "NewSNS must return a *SNS")
		})
	}
}

func TestNewSNSMalformedConfig(t *testing.T) {
	_, err := NewSNSWithClient(json.RawMessage(`{"topic_arn": }`), &mockSNSClient{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestNewSNSMissingTopicARN(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"empty object", `{}`},
		{"empty topic_arn", `{"topic_arn": "", "region": "us-east-1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSNSWithClient(json.RawMessage(tt.config), &mockSNSClient{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "topic_arn is required")
		})
	}
}

func TestNewSNSMissingRegion(t *testing.T) {
	_, err := NewSNSWithClient(json.RawMessage(`{"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon"}`), &mockSNSClient{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "region is required")
}

func TestNewSNSInvalidTopicARN(t *testing.T) {
	_, err := NewSNSWithClient(json.RawMessage(`{"topic_arn": "invalid-arn", "region": "us-east-1"}`), &mockSNSClient{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a valid SNS topic ARN")
}

func TestSNSSendSuccess(t *testing.T) {
	var capturedMessage string
	mockClient := &mockSNSClient{
		publishFunc: func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
			capturedMessage = aws.ToString(params.Message)
			return &sns.PublishOutput{MessageId: aws.String("test-message-id")}, nil
		},
	}

	n, err := NewSNSWithClient(json.RawMessage(`{"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon", "region": "us-east-1"}`), mockClient)
	require.NoError(t, err)

	alert := Alert{
		ID:          7,
		MonitorID:   9,
		MonitorName: "my-monitor",
		RuleID:      3,
		RuleType:    "event_emitted",
		ContractID:  "CABC123",
		EventID:     "ev-1",
		EventName:   "transfer",
		Ledger:      3721765,
		TxHash:      "4f2a...",
		Payload:     json.RawMessage(`{"amount": "1000"}`),
		Severity:    "warning",
	}
	require.NoError(t, n.Send(context.Background(), alert))

	require.NotEmpty(t, capturedMessage)
	var msg snsMessage
	require.NoError(t, json.Unmarshal([]byte(capturedMessage), &msg))
	assert.Equal(t, int64(9), msg.MonitorID)
	assert.Equal(t, "my-monitor", msg.MonitorName)
	assert.Equal(t, int64(3), msg.RuleID)
	assert.Equal(t, "event_emitted", msg.RuleType)
	assert.Equal(t, "ev-1", msg.EventID)
	assert.Equal(t, "CABC123", msg.ContractID)
	assert.Equal(t, "transfer", msg.EventName)
	assert.Equal(t, uint32(3721765), msg.Ledger)
	assert.Equal(t, "4f2a...", msg.TxHash)
	assert.Equal(t, "warning", msg.Severity)
	var payloadMap map[string]any
	require.NoError(t, json.Unmarshal(msg.Payload, &payloadMap))
	assert.Equal(t, "1000", payloadMap["amount"])
	assert.NotEmpty(t, msg.CreatedAt)
}

func TestSNSSendFailure(t *testing.T) {
	mockClient := &mockSNSClient{
		publishFunc: func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
			return nil, &types.InvalidParameterException{Message: aws.String("Invalid topic ARN")}
		},
	}

	n, err := NewSNSWithClient(json.RawMessage(`{"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon", "region": "us-east-1"}`), mockClient)
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleID: 2, EventID: "e", ContractID: "c", RuleType: "r"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sns:")
}

func TestSNSCredentialsNotLogged(t *testing.T) {
	const secretKey = "super-secret-access-key"
	mockClient := &mockSNSClient{
		publishFunc: func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
			return nil, &types.InternalErrorException{Message: aws.String("internal error")}
		},
	}

	n, err := NewSNSWithClient(json.RawMessage(fmt.Sprintf(`{"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon", "region": "us-east-1", "access_key_id": "AKIA...", "secret_access_key": %q}`, secretKey)), mockClient)
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{ID: 1, MonitorName: "m", RuleID: 2, EventID: "e", ContractID: "c", RuleType: "r"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretKey, "errors must never leak the secret_access_key")
}

func TestSNSMessageStructure(t *testing.T) {
	var capturedMessage string
	mockClient := &mockSNSClient{
		publishFunc: func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
			capturedMessage = aws.ToString(params.Message)
			return &sns.PublishOutput{MessageId: aws.String("test-message-id")}, nil
		},
	}

	n, err := NewSNSWithClient(json.RawMessage(`{"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon", "region": "us-east-1"}`), mockClient)
	require.NoError(t, err)

	alert := Alert{
		ID:          1,
		MonitorID:   1,
		MonitorName: "test-monitor",
		RuleID:      1,
		RuleType:    "value_threshold",
		ContractID:  "CONTRACT123",
		EventID:     "event-456",
		EventName:   "threshold_breached",
		Ledger:      12345,
		TxHash:      "tx-hash-789",
		Severity:    "critical",
	}
	require.NoError(t, n.Send(context.Background(), alert))

	// Verify the message is valid JSON and contains all expected fields
	var msg map[string]any
	require.NoError(t, json.Unmarshal([]byte(capturedMessage), &msg))

	expectedFields := []string{
		"monitor_id", "monitor_name", "rule_id", "rule_type",
		"event_id", "contract_id", "event_name", "ledger",
		"tx_hash", "payload", "created_at", "severity",
	}
	for _, field := range expectedFields {
		assert.Contains(t, msg, field, "message must contain %s", field)
	}

	// Verify payload is included even when empty (nil serializes to null in JSON)
	assert.Nil(t, msg["payload"])
}

// withHermeticAWSEnvironment removes every ambient credential source before a
// test builds a real SDK client. The default chain falls back to the EC2/ECS
// metadata endpoint when nothing else is configured, and a lookup there is an
// HTTP request with a timeout — slow and flaky on a machine that is not on
// AWS. Disabling it and pointing the shared-config files at /dev/null keeps
// construction tests offline by construction rather than by luck.
func withHermeticAWSEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_PROFILE",
		"AWS_DEFAULT_PROFILE",
		"AWS_REGION",
		"AWS_DEFAULT_REGION",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN",
		"AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_ROLE_ARN",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// TestNewSNSBuildsNotifierFromChannelConfig covers the construction path the
// channel factory actually calls. Static credentials are supplied so the SDK
// never consults the default chain; the configuration it produces is then
// asserted directly, including the region and topic the notifier will use.
func TestNewSNSBuildsNotifierFromChannelConfig(t *testing.T) {
	withHermeticAWSEnvironment(t)

	n, err := NewSNS(json.RawMessage(`{
		"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon",
		"region": "us-east-1",
		"access_key_id": "AKIAIOSFODNN7EXAMPLE",
		"secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	}`))
	require.NoError(t, err)

	s, ok := n.(*SNS)
	require.True(t, ok, "NewSNS must return a *SNS")
	assert.Equal(t, "arn:aws:sns:us-east-1:123456789012:sorobeacon", s.cfg.TopicARN)
	assert.Equal(t, "us-east-1", s.cfg.Region)
	require.NotNil(t, s.client)

	client, ok := s.client.(*sns.Client)
	require.True(t, ok, "NewSNS must build a real SNS client")
	assert.Equal(t, "us-east-1", client.Options().Region)
	require.NotNil(t, client.Options().Credentials,
		"the SDK must have a credentials provider even when it has not resolved one yet")
}

// TestNewSNSDefersCredentialResolution pins the deliberate choice: when no
// static credentials are configured, construction succeeds and the SDK's
// default chain resolves credentials later, per request. Failing here would
// reject a valid instance-role deployment whose credentials appear after
// boot, and the SDK refreshes them for the process's lifetime.
//
// Retrieval is asserted offline: metadata is disabled and both shared-config
// files are /dev/null, so the chain reports "no credentials" without a
// network round trip.
func TestNewSNSDefersCredentialResolution(t *testing.T) {
	withHermeticAWSEnvironment(t)

	n, err := NewSNS(json.RawMessage(`{
		"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon",
		"region": "us-east-1"
	}`))
	require.NoError(t, err, "construction must defer credential resolution, not fail")

	s, ok := n.(*SNS)
	require.True(t, ok)
	client, ok := s.client.(*sns.Client)
	require.True(t, ok)
	require.NotNil(t, client.Options().Credentials,
		"the default chain must still be configured even though no credential is available")

	_, err = client.Options().Credentials.Retrieve(context.Background())
	require.Error(t, err, "the hermetic environment has no credentials to resolve")
}

// TestNewSNSClientInjectsStaticCredentials proves the static pair reaches the
// SDK as a credentials provider, rather than being accepted and ignored.
func TestNewSNSClientInjectsStaticCredentials(t *testing.T) {
	withHermeticAWSEnvironment(t)

	client, err := newSNSClient(snsConfig{
		Region:          "eu-west-1",
		AccessKeyID:     "AKIASTATICEXAMPLE",
		SecretAccessKey: "static-secret-value",
	})
	require.NoError(t, err)

	creds, err := client.(*sns.Client).Options().Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "AKIASTATICEXAMPLE", creds.AccessKeyID)
	assert.Equal(t, "static-secret-value", creds.SecretAccessKey)
}

// TestNewSNSClientRequiresBothStaticCredentialFields documents a sharp edge:
// the code injects static credentials only when both fields are present. A
// half-configured pair is silently ignored and the default chain takes over,
// so a deployment can authenticate as a different identity than the operator
// configured. The behaviour is pinned here rather than changed, because
// validating it belongs with the config review in a separate issue.
func TestNewSNSClientRequiresBothStaticCredentialFields(t *testing.T) {
	withHermeticAWSEnvironment(t)

	client, err := newSNSClient(snsConfig{
		Region:      "us-east-1",
		AccessKeyID: "AKIAHALFEXAMPLE",
	})
	require.NoError(t, err, "a partial static pair is not a construction error")

	_, err = client.(*sns.Client).Options().Credentials.Retrieve(context.Background())
	require.Error(t, err, "the partial pair must not be used as credentials")
}

// TestNewSNSAcceptsRegionThatDisagreesWithTopicARN pins the documented choice:
// the region field is authoritative and the ARN's embedded region is not
// cross-checked. A mismatch therefore surfaces as a provider-side error on
// the first publish. docs/channels/sns.md states this explicitly.
func TestNewSNSAcceptsRegionThatDisagreesWithTopicARN(t *testing.T) {
	withHermeticAWSEnvironment(t)

	n, err := NewSNS(json.RawMessage(`{
		"topic_arn": "arn:aws:sns:eu-west-1:123456789012:sorobeacon",
		"region": "us-east-1",
		"access_key_id": "AKIAIOSFODNN7EXAMPLE",
		"secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	}`))
	require.NoError(t, err)

	client := n.(*SNS).client.(*sns.Client)
	assert.Equal(t, "us-east-1", client.Options().Region,
		"the region field, not the ARN, selects the endpoint")
}

// TestNewSNSValidatesChannelConfig covers the public constructor's rejection
// path. None of these inputs reach client construction, and every error names
// the offending field so an operator can fix the channel config.
func TestNewSNSValidatesChannelConfig(t *testing.T) {
	withHermeticAWSEnvironment(t)

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "missing topic arn",
			config:  `{"region": "us-east-1"}`,
			wantErr: "topic_arn is required",
		},
		{
			name:    "missing region",
			config:  `{"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon"}`,
			wantErr: "region is required",
		},
		{
			name:    "not an sns arn",
			config:  `{"topic_arn": "not-an-arn", "region": "us-east-1"}`,
			wantErr: "topic_arn must be a valid SNS topic ARN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSNS(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestNewSNSConstructionErrorsDoNotLeakCredentials is the standing rule
// applied to the construction path: a bad channel config must not echo the
// credentials it contains back through the API or the logs.
func TestNewSNSConstructionErrorsDoNotLeakCredentials(t *testing.T) {
	withHermeticAWSEnvironment(t)

	const (
		accessKey = "AKIACONSTRUCTIONEXAMPLE"
		secretKey = "construction-secret-value-do-not-leak"
	)

	tests := []struct {
		name   string
		config string
	}{
		{
			name: "malformed json",
			config: fmt.Sprintf(`{
				"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon",
				"region": "us-east-1",
				"access_key_id": %q,
				"secret_access_key": %q,
			}`, accessKey, secretKey),
		},
		{
			name: "missing region",
			config: fmt.Sprintf(`{
				"topic_arn": "arn:aws:sns:us-east-1:123456789012:sorobeacon",
				"access_key_id": %q,
				"secret_access_key": %q
			}`, accessKey, secretKey),
		},
		{
			name: "not an arn",
			config: fmt.Sprintf(`{
				"topic_arn": "not-an-arn",
				"region": "us-east-1",
				"access_key_id": %q,
				"secret_access_key": %q
			}`, accessKey, secretKey),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSNS(json.RawMessage(tt.config))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), secretKey)
			assert.NotContains(t, err.Error(), accessKey)
		})
	}
}
