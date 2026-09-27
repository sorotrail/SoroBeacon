package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// matrixConfig: {"homeserver_url": "https://matrix.example.org",
// "access_token": "syt_...", "room_id": "!abc:example.org"}
type matrixConfig struct {
	HomeserverURL string `json:"homeserver_url"`
	AccessToken   string `json:"access_token"`
	RoomID        string `json:"room_id"`
}

// Matrix sends alerts into a Matrix room through the client-server API.
//
// The message is sent with PUT rather than POST so the transaction ID in the
// URL makes delivery idempotent: a retry of the same alert reuses the same
// transaction ID, and the homeserver returns the original event instead of
// posting the message twice. The transaction ID is derived from the alert ID
// for exactly that reason.
type Matrix struct {
	cfg matrixConfig
}

// NewMatrix builds a Matrix notifier from channel config.
func NewMatrix(config json.RawMessage) (Notifier, error) {
	var cfg matrixConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("matrix: invalid config: %w", err)
	}
	if cfg.HomeserverURL == "" || cfg.AccessToken == "" || cfg.RoomID == "" {
		return nil, fmt.Errorf("matrix: homeserver_url, access_token and room_id are required")
	}
	cfg.HomeserverURL = strings.TrimRight(cfg.HomeserverURL, "/")
	return &Matrix{cfg: cfg}, nil
}

func (m *Matrix) Send(ctx context.Context, a Alert) error {
	msg, err := RenderText(a)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"msgtype": "m.text",
		"body":    msg,
	})
	if err != nil {
		return err
	}

	// The room ID and transaction ID are path segments: a room ID contains
	// "!" and ":" and a malformed or empty alert ID must not escape the
	// path, so escape both.
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		m.cfg.HomeserverURL, url.PathEscape(m.cfg.RoomID), url.PathEscape(strconv.FormatInt(a.ID, 10)))

	// The access token is a bearer credential: it travels in a header, never
	// in the URL, so it cannot leak into a transport error or a log line.
	headers := map[string]string{"Authorization": "Bearer " + m.cfg.AccessToken}
	if err := requestJSON(ctx, "PUT", endpoint, body, headers); err != nil {
		return fmt.Errorf("matrix: %w", err)
	}
	return nil
}
