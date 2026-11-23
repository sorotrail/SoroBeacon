package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// googleChatConfig holds the configuration for the Google Chat notification channel.
type googleChatConfig struct {
	WebhookURL string `json:"webhook_url"`
}

// googleChatCard represents the cardsV2 message structure for Google Chat.
type googleChatCard struct {
	CardsV2 []cardV2 `json:"cardsV2"`
}

type cardV2 struct {
	CardID string `json:"cardId"`
	Card   card   `json:"card"`
}

type card struct {
	Header   cardHeader    `json:"header"`
	Sections []cardSection `json:"sections"`
}

type cardHeader struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	ImageURL string `json:"imageUrl,omitempty"`
}

type cardSection struct {
	Widgets []widget `json:"widgets"`
}

type widget struct {
	KeyValue *keyValue `json:"keyValue,omitempty"`
}

type keyValue struct {
	TopLabel string `json:"topLabel"`
	Content  string `json:"content"`
}

// GoogleChat posts alerts to a Google Chat space via an incoming webhook.
type GoogleChat struct {
	cfg googleChatConfig
}

// NewGoogleChat builds a Google Chat notifier from channel config.
func NewGoogleChat(config json.RawMessage) (Notifier, error) {
	var cfg googleChatConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("googlechat: invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("googlechat: webhook_url is required")
	}
	if !strings.HasPrefix(cfg.WebhookURL, "https://") && !strings.HasPrefix(cfg.WebhookURL, "http://localhost") && !strings.HasPrefix(cfg.WebhookURL, "http://127.0.0.1") {
		return nil, fmt.Errorf("googlechat: webhook_url must use HTTPS")
	}
	return &GoogleChat{cfg: cfg}, nil
}

func (g *GoogleChat) Send(ctx context.Context, a Alert) error {
	card := buildCard(a)
	body, err := json.Marshal(card)
	if err != nil {
		return err
	}
	if err := postJSON(ctx, g.cfg.WebhookURL, body, nil); err != nil {
		return fmt.Errorf("googlechat: %w", err)
	}
	return nil
}

func buildCard(a Alert) googleChatCard {
	widgets := []widget{
		{KeyValue: &keyValue{TopLabel: "Monitor", Content: a.MonitorName}},
		{KeyValue: &keyValue{TopLabel: "Rule Type", Content: a.RuleType}},
		{KeyValue: &keyValue{TopLabel: "Contract ID", Content: a.ContractID}},
		{KeyValue: &keyValue{TopLabel: "Event ID", Content: a.EventID}},
		{KeyValue: &keyValue{TopLabel: "Timestamp", Content: a.CreatedAt.UTC().Format("2006-01-02 15:04:05") + " UTC"}},
	}
	if a.EventName != "" {
		widgets = append([]widget{{KeyValue: &keyValue{TopLabel: "Event", Content: a.EventName}}}, widgets...)
	}

	return googleChatCard{
		CardsV2: []cardV2{
			{
				CardID: "sorobeacon-alert",
				Card: card{
					Header: cardHeader{
						Title:    "SoroBeacon Alert",
						Subtitle: a.MonitorName,
					},
					Sections: []cardSection{
						{Widgets: widgets},
					},
				},
			},
		},
	}
}
