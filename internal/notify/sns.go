package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

// snsConfig holds the configuration for the AWS SNS notification channel.
type snsConfig struct {
	TopicARN        string `json:"topic_arn"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

// SNSAPI defines the interface for the SNS client, allowing for mocking in tests.
type SNSAPI interface {
	Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

// SNS publishes alerts to an AWS SNS topic using SigV4 signing.
type SNS struct {
	cfg    snsConfig
	client SNSAPI
}

// NewSNS builds an SNS notifier from channel config.
func NewSNS(config json.RawMessage) (Notifier, error) {
	return NewSNSWithClient(config, nil)
}

// NewSNSWithClient builds an SNS notifier with a custom client (for testing).
func NewSNSWithClient(config json.RawMessage, client SNSAPI) (Notifier, error) {
	var cfg snsConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("sns: invalid config: %w", err)
	}
	if cfg.TopicARN == "" {
		return nil, fmt.Errorf("sns: topic_arn is required")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("sns: region is required")
	}

	// Validate ARN format (basic check)
	if !strings.HasPrefix(cfg.TopicARN, "arn:aws:sns:") {
		return nil, fmt.Errorf("sns: topic_arn must be a valid SNS topic ARN")
	}

	var snsClient SNSAPI
	if client != nil {
		snsClient = client
	} else {
		var err error
		snsClient, err = newSNSClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("sns: failed to create client: %w", err)
		}
	}

	return &SNS{cfg: cfg, client: snsClient}, nil
}

func newSNSClient(cfg snsConfig) (SNSAPI, error) {
	var opts []func(*config.LoadOptions) error
	opts = append(opts, config.WithRegion(cfg.Region))

	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
				return aws.Credentials{
					AccessKeyID:     cfg.AccessKeyID,
					SecretAccessKey: cfg.SecretAccessKey,
				}, nil
			}),
		))
	}

	sdkConfig, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	return sns.NewFromConfig(sdkConfig), nil
}

func (s *SNS) Send(ctx context.Context, a Alert) error {
	message := buildSNSMessage(a)
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}

	_, err = s.client.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(s.cfg.TopicARN),
		Message:  aws.String(string(body)),
	})
	if err != nil {
		return fmt.Errorf("sns: %w", err)
	}
	return nil
}

type snsMessage struct {
	MonitorID   int64           `json:"monitor_id"`
	MonitorName string          `json:"monitor_name"`
	RuleID      int64           `json:"rule_id"`
	RuleType    string          `json:"rule_type"`
	EventID     string          `json:"event_id"`
	ContractID  string          `json:"contract_id"`
	EventName   string          `json:"event_name,omitempty"`
	Ledger      uint32          `json:"ledger"`
	TxHash      string          `json:"tx_hash"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   string          `json:"created_at"`
	Severity    string          `json:"severity,omitempty"`
}

func buildSNSMessage(a Alert) snsMessage {
	return snsMessage{
		MonitorID:   a.MonitorID,
		MonitorName: a.MonitorName,
		RuleID:      a.RuleID,
		RuleType:    a.RuleType,
		EventID:     a.EventID,
		ContractID:  a.ContractID,
		EventName:   a.EventName,
		Ledger:      a.Ledger,
		TxHash:      a.TxHash,
		Payload:     a.Payload,
		CreatedAt:   a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Severity:    a.Severity,
	}
}

// Ensure sns.Client implements SNSAPI.
var _ SNSAPI = (*sns.Client)(nil)
