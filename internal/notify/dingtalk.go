package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// dingtalkConfig: {"webhook_url": "https://oapi.dingtalk.com/robot/send?access_token=...", "secret": "SEC..."}
type dingtalkConfig struct {
	WebhookURL string `json:"webhook_url"`
	Secret     string `json:"secret,omitempty"`
	Template   string `json:"template,omitempty"`
}

// DingTalk posts alerts to a DingTalk robot webhook using markdown format.
type DingTalk struct {
	cfg dingtalkConfig
	tpl channelTemplate
}

// NewDingTalk builds a DingTalk notifier from channel config.
func NewDingTalk(config json.RawMessage) (Notifier, error) {
	var cfg dingtalkConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("dingtalk: invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("dingtalk: webhook_url is required")
	}
	if !strings.HasPrefix(cfg.WebhookURL, "https://") {
		return nil, fmt.Errorf("dingtalk: webhook_url must use HTTPS")
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("dingtalk: %w", err)
	}
	return &DingTalk{cfg: cfg, tpl: tpl}, nil
}

func (d *DingTalk) Send(ctx context.Context, a Alert) error {
	msg, err := d.tpl.render(a)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": "SoroBeacon Alert",
			"text":  msg,
		},
	})
	if err != nil {
		return err
	}

	sendURL := d.cfg.WebhookURL
	if d.cfg.Secret != "" {
		timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
		sign := dingtalkSign(d.cfg.Secret, timestamp)
		parsed, err := url.Parse(sendURL)
		if err != nil {
			return fmt.Errorf("dingtalk: parse webhook_url: %w", err)
		}
		q := parsed.Query()
		q.Set("timestamp", timestamp)
		q.Set("sign", sign)
		parsed.RawQuery = q.Encode()
		sendURL = parsed.String()
	}

	respBody, err := postJSONWithResponse(ctx, sendURL, body, nil)
	if err != nil {
		return fmt.Errorf("dingtalk: %w", err)
	}

	// DingTalk returns HTTP 200 with a non-zero errcode on failure.
	var dingtalkResp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(respBody, &dingtalkResp); err == nil && dingtalkResp.ErrCode != 0 {
		return fmt.Errorf("dingtalk: errcode %d: %s", dingtalkResp.ErrCode, dingtalkResp.ErrMsg)
	}

	return nil
}

// dingtalkSign computes the base64-encoded HMAC-SHA256 signature as documented
// by DingTalk: sign = base64(hmac_sha256(secret, timestamp + "\n" + secret)).
func dingtalkSign(secret, timestamp string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}