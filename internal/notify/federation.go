package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TypeFederation is the channel type name for cross-instance alert forwarding.
const TypeFederation = "federation"

// MaxHopCount bounds the number of times a federated alert can be forwarded.
// The receiving instance rejects an alert that reaches this count, so two
// instances federating to each other terminate rather than amplifying forever.
const MaxHopCount = 8

// federationConfig holds the upstream SoroBeacon instance's ingest URL and
// the bearer token used to authenticate against it.
type federationConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// FederatedAlert is the wire format for the ingest endpoint. It wraps a
// normal alert with origin and hop tracking so the receiving instance can
// attribute the alert and prevent loops.
type FederatedAlert struct {
	Alert    Alert  `json:"alert"`
	Origin   string `json:"origin"`
	HopCount int    `json:"hop_count"`
}

// Federation forwards alerts to another SoroBeacon instance's /api/v1/ingest
// endpoint. The receiving instance stores the alert as a federated alert,
// marked with its origin, without re-evaluating rules.
type Federation struct {
	cfg    federationConfig
	origin string
}

// NewFederationWithOrigin builds a Federation notifier from channel config
// and the sending instance's origin. The origin is required: the receiving
// instance rejects a federated alert without one.
func NewFederationWithOrigin(config json.RawMessage, origin string) (Notifier, error) {
	var cfg federationConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("federation: invalid config: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("federation: url is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("federation: token is required")
	}
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return nil, fmt.Errorf("federation: INSTANCE_NAME is required to send federated alerts")
	}
	return &Federation{cfg: cfg, origin: origin}, nil
}

// newFederation is the factory constructor. It reads the origin configured on
// the Factory rather than a per-channel value, so every channel from the same
// instance identifies itself the same way.
func (f *Factory) newFederation(config json.RawMessage) (Notifier, error) {
	return NewFederationWithOrigin(config, f.federationOrigin)
}

func (f *Federation) Send(ctx context.Context, a Alert) error {
	if f.origin == "" {
		return fmt.Errorf("federation: INSTANCE_NAME is required to send federated alerts")
	}
	fa := FederatedAlert{
		Alert:    a,
		Origin:   f.origin,
		HopCount: 1,
	}
	body, err := json.Marshal(fa)
	if err != nil {
		return fmt.Errorf("federation: marshal: %w", err)
	}
	headers := map[string]string{
		"Authorization": "Bearer " + f.cfg.Token,
	}
	ingestURL := f.cfg.URL
	if ingestURL[len(ingestURL)-1] != '/' {
		ingestURL += "/"
	}
	ingestURL += "api/v1/ingest"

	if err := postJSON(ctx, ingestURL, body, headers); err != nil {
		return fmt.Errorf("federation: %w", err)
	}
	return nil
}
