package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// emailConfig:
//
//	{
//	  "host": "smtp.example.com", "port": 587,
//	  "username": "beacon", "password": "...",
//	  "from": "beacon@example.com", "to": ["ops@example.com"]
//	}
type emailConfig struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	From     string   `json:"from"`
	To       []string `json:"to"`
}

// Email sends alerts over SMTP (STARTTLS via net/smtp when offered).
type Email struct {
	cfg emailConfig
}

// NewEmail builds an Email notifier from channel config.
func NewEmail(config json.RawMessage) (Notifier, error) {
	var cfg emailConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("email: invalid config: %w", err)
	}
	if cfg.Host == "" || cfg.From == "" || len(cfg.To) == 0 {
		return nil, fmt.Errorf("email: host, from and to are required")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	return &Email{cfg: cfg}, nil
}

func (e *Email) Send(ctx context.Context, a Alert) error {
	msg, err := RenderText(a)
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("SoroBeacon alert: %s", a.MonitorName)
	body := strings.Join([]string{
		"From: " + e.cfg.From,
		"To: " + strings.Join(e.cfg.To, ", "),
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		msg,
	}, "\r\n")

	addr := net.JoinHostPort(e.cfg.Host, fmt.Sprintf("%d", e.cfg.Port))
	var auth smtp.Auth
	if e.cfg.Username != "" {
		auth = smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.Host)
	}

	// net/smtp has no context support; honor ctx cancellation coarsely.
	done := make(chan error, 1)
	go func() {
		done <- smtp.SendMail(addr, auth, e.cfg.From, e.cfg.To, []byte(body))
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("email: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("email: %w", ctx.Err())
	}
}
