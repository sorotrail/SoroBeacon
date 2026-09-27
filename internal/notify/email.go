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
//	  "from": "beacon@example.com", "to": ["ops@example.com"],
//	  "subject_prefix": "[PROD] "
//	}
type emailConfig struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	// SubjectPrefix is prepended to the subject verbatim when set, so a
	// team routing mail through filters can key on a tag like "[PROD] "
	// (the space, if wanted, is the caller's to include). Empty by
	// default, which leaves the subject exactly as before this option
	// existed.
	SubjectPrefix string `json:"subject_prefix"`
	// Template optionally overrides the plain-text *body*; empty uses the
	// shared default (see RenderText and docs/channels/templates.md). The
	// subject is built separately from SubjectPrefix.
	Template string `json:"template,omitempty"`
}

// Email sends alerts over SMTP (STARTTLS via net/smtp when offered).
type Email struct {
	cfg emailConfig
	tpl channelTemplate
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
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("email: %w", err)
	}
	return &Email{cfg: cfg, tpl: tpl}, nil
}

// subject builds the alert's Subject header, honoring the configured
// prefix. Split out from Send so it's testable without a real SMTP
// connection.
func (e *Email) subject(a Alert) string {
	return e.cfg.SubjectPrefix + fmt.Sprintf("SoroBeacon alert: %s", a.MonitorName)
}

// body builds the full RFC 5322 message for an alert. Like subject it is
// split out from Send so the rendered body (including a custom template) is
// testable without a real SMTP connection.
func (e *Email) body(a Alert) ([]byte, error) {
	msg, err := e.tpl.render(a)
	if err != nil {
		return nil, err
	}
	return []byte(strings.Join([]string{
		"From: " + e.cfg.From,
		"To: " + strings.Join(e.cfg.To, ", "),
		"Subject: " + e.subject(a),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		msg,
	}, "\r\n")), nil
}

func (e *Email) Send(ctx context.Context, a Alert) error {
	body, err := e.body(a)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(e.cfg.Host, fmt.Sprintf("%d", e.cfg.Port))
	var auth smtp.Auth
	if e.cfg.Username != "" {
		auth = smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.Host)
	}

	// net/smtp has no context support; honor ctx cancellation coarsely.
	done := make(chan error, 1)
	go func() {
		done <- smtp.SendMail(addr, auth, e.cfg.From, e.cfg.To, body)
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
