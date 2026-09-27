package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// TypeFederation is the channel type name for cross-instance alert forwarding.
const TypeFederation = "federation"

// MaxHopCount bounds the number of times a federated alert can be forwarded.
// Two instances federating to each other terminate after this many hops
// rather than amplifying forever.
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

// NewFederation builds a Federation notifier from channel config.
func NewFederation(config json.RawMessage) (Notifier, error) {
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
	return &Federation{cfg: cfg, origin: ""}, nil
}

// NewFederationWithOrigin builds a Federation notifier with an explicit origin
// identifier. Used when the sending instance knows its own identity.
func NewFederationWithOrigin(config json.RawMessage, origin string) (Notifier, error) {
	n, err := NewFederation(config)
	if err != nil {
		return nil, err
	}
	n.(*Federation).origin = origin
	return n, nil
}

func (f *Federation) Send(ctx context.Context, a Alert) error {
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

// ForwardFederatedAlert re-forwards an already-federated alert to the next
// hop, incrementing the hop count. Returns an error if the hop limit is
// reached.
func ForwardFederatedAlert(ctx context.Context, f *Federation, fa FederatedAlert) error {
	if fa.HopCount >= MaxHopCount {
		return fmt.Errorf("federation: hop limit %d reached; dropping alert to prevent loop", MaxHopCount)
	}
	fa.HopCount++
	body, err := json.Marshal(fa)
	if err != nil {
		return fmt.Errorf("federation: marshal: %w", err)
	}
	headers := map[string]string{
		"Authorization":  "Bearer " + f.cfg.Token,
		"Content-Type":   "application/json",
		http.CanonicalHeaderKey("X-Federation-Hop"): fmt.Sprintf("%d", fa.HopCount),
	}
	ingestURL := f.cfg.URL
	if ingestURL[len(ingestURL)-1] != '/' {
		ingestURL += "/"
	}
	ingestURL += "api/v1/ingest"

	return requestJSON(ctx, http.MethodPost, ingestURL, body, headers)
}
