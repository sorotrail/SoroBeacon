package notify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
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
	// RequireTLS refuses to deliver unless the SMTP session is encrypted.
	// STARTTLS is opportunistic in the protocol, so a relay that stops
	// advertising it silently downgrades the session to plaintext and the
	// alert — and the credentials that travel with it — cross the network
	// in the clear. With this flag the client refuses the delivery instead
	// of downgrading.
	RequireTLS bool `json:"require_tls"`
	// TLSSkipVerify turns off certificate verification for the SMTP
	// session. It exists for relays that present a self-signed
	// certificate; the session is still encrypted, but the peer's identity
	// is no longer checked. Verification stays on unless an operator asks
	// for it explicitly.
	TLSSkipVerify bool `json:"tls_skip_verify"`
	// ConnectTimeoutSeconds bounds establishing the session: the TCP
	// connect and the SMTP greeting. Zero means the default.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds"`
	// TimeoutSeconds bounds the whole delivery, greeting included. Zero
	// means the default.
	TimeoutSeconds int `json:"timeout_seconds"`
}

const (
	// defaultEmailPort is the submission port used when the config does
	// not name one.
	defaultEmailPort = 587
	// defaultEmailConnectTimeout bounds establishing the SMTP session. A
	// server that accepts the connection and never sends its greeting must
	// not hold the delivery goroutine for longer than this.
	defaultEmailConnectTimeout = 10 * time.Second
	// defaultEmailTimeout bounds the whole SMTP conversation.
	defaultEmailTimeout = 30 * time.Second
	// maxEmailTimeout caps a configured timeout. The channel's own timeout
	// is the last line of defence against a server that never answers, so
	// it is bounded rather than trusted.
	maxEmailTimeout = 10 * time.Minute
)

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
		cfg.Port = defaultEmailPort
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("email: port must be between 1 and 65535")
	}
	// A line break in an address or in the subject prefix ends the header
	// it sits in, and whatever follows becomes a header of its own. The
	// values are channel config, so they are refused here rather than
	// quietly rewritten into a message that says something else.
	if err := validateHeaderLine("from", cfg.From); err != nil {
		return nil, err
	}
	for _, to := range cfg.To {
		if err := validateHeaderLine("to", to); err != nil {
			return nil, err
		}
	}
	if err := validateHeaderLine("subject_prefix", cfg.SubjectPrefix); err != nil {
		return nil, err
	}
	if cfg.ConnectTimeoutSeconds < 0 || time.Duration(cfg.ConnectTimeoutSeconds)*time.Second > maxEmailTimeout {
		return nil, fmt.Errorf("email: connect_timeout_seconds must be between 0 and %d", int(maxEmailTimeout/time.Second))
	}
	if cfg.TimeoutSeconds < 0 || time.Duration(cfg.TimeoutSeconds)*time.Second > maxEmailTimeout {
		return nil, fmt.Errorf("email: timeout_seconds must be between 0 and %d", int(maxEmailTimeout/time.Second))
	}
	tpl, err := parseChannelTemplate(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("email: %w", err)
	}
	return &Email{cfg: cfg, tpl: tpl}, nil
}

// validateHeaderLine refuses a config value that would break out of the
// header it is placed in. The value itself is not quoted back: it is channel
// config, and the rule that keeps webhook URLs and bot tokens out of error
// messages keeps addresses out too.
func validateHeaderLine(field, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("email: %s must not contain a line break", field)
	}
	return nil
}

// subject builds the alert's Subject header, honoring the configured
// prefix. Split out from Send so it's testable without a real SMTP
// connection.
func (e *Email) subject(a Alert) string {
	return e.cfg.SubjectPrefix + fmt.Sprintf("SoroBeacon alert: %s", sanitizeHeaderValue(a.MonitorName))
}

// sanitizeHeaderValue removes the characters that would end a header line.
// The subject carries the monitor name, and the body carries text that
// arrived with the contract, so a newline in either would let the value add
// headers of its own — a Bcc, for instance. A CRLF pair counts as one line
// break, so the value keeps its shape instead of gaining a second space.
func sanitizeHeaderValue(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, value)
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

// sendTimeout is the ceiling on the whole SMTP conversation.
func (e *Email) sendTimeout() time.Duration {
	if e.cfg.TimeoutSeconds > 0 {
		return time.Duration(e.cfg.TimeoutSeconds) * time.Second
	}
	return defaultEmailTimeout
}

// connectTimeout is the ceiling on establishing the session: the TCP connect
// and the SMTP greeting. It never exceeds the overall timeout.
func (e *Email) connectTimeout(sendTimeout time.Duration) time.Duration {
	timeout := defaultEmailConnectTimeout
	if e.cfg.ConnectTimeoutSeconds > 0 {
		timeout = time.Duration(e.cfg.ConnectTimeoutSeconds) * time.Second
	}
	if timeout > sendTimeout {
		return sendTimeout
	}
	return timeout
}

// tlsConfig builds the TLS settings for the SMTP session. The server name is
// the configured host, so the relay's certificate has to be valid for it.
func (e *Email) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName: e.cfg.Host,
		MinVersion: tls.VersionTLS12,
		// Verification stays on unless the operator explicitly turned it
		// off for a relay with a self-signed certificate.
		InsecureSkipVerify: e.cfg.TLSSkipVerify, // #nosec G402 -- opt-in per-channel setting for internal SMTP relays with self-signed certificates
	}
}

// Check establishes an SMTP session and verifies TLS and authentication,
// then quits before issuing MAIL, RCPT or DATA. It never sends a message.
func (e *Email) Check(ctx context.Context) error {
	timeout := e.sendTimeout()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	connectTimeout := e.connectTimeout(timeout)
	addr := net.JoinHostPort(e.cfg.Host, strconv.Itoa(e.cfg.Port))
	conn, err := (&net.Dialer{Timeout: connectTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("email: connect: %w", e.sanitizeError(err))
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("email: %w", e.sanitizeError(err))
		}
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopped:
		}
	}()
	client, err := smtp.NewClient(conn, e.cfg.Host)
	if err != nil {
		return fmt.Errorf("email: greeting: %w", e.sanitizeError(err))
	}
	defer func() { _ = client.Close() }()
	if offered, _ := client.Extension("STARTTLS"); offered {
		if err := client.StartTLS(e.tlsConfig()); err != nil {
			return fmt.Errorf("email: starttls: %w", e.sanitizeError(err))
		}
	} else if e.cfg.RequireTLS {
		return errors.New("email: the server does not offer STARTTLS but the channel requires TLS")
	}
	if e.cfg.Username != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.New("email: the server does not offer authentication but the channel is configured with a username")
		}
		if err := client.Auth(smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.Host)); err != nil {
			return fmt.Errorf("email: authentication failed: %w", e.sanitizeError(err))
		}
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("email: quit: %w", e.sanitizeError(err))
	}
	return nil
}

// Send delivers one alert over SMTP.
//
// The conversation is bounded twice over: the dispatcher gives every
// delivery a deadline, and the channel has its own timeouts for the
// connection and for the send. net/smtp has no deadline of its own, so a
// server that accepts the connection and then stops answering would
// otherwise hold the delivery goroutine for as long as the process lives.
func (e *Email) Send(ctx context.Context, a Alert) error {
	body, err := e.body(a)
	if err != nil {
		return err
	}

	sendTimeout := e.sendTimeout()
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < sendTimeout {
			sendTimeout = remaining
		}
	}
	if sendTimeout <= 0 {
		return fmt.Errorf("email: %w", context.DeadlineExceeded)
	}
	connectTimeout := e.connectTimeout(sendTimeout)

	addr := net.JoinHostPort(e.cfg.Host, strconv.Itoa(e.cfg.Port))
	dialer := &net.Dialer{Timeout: connectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("email: connect: %w", e.sanitizeError(err))
	}
	defer func() { _ = conn.Close() }()

	// Cancelling the context has to interrupt a read that is already in
	// flight; closing the connection is the only way to do that.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopped:
		}
	}()

	// The greeting is the last of the connection phase, so it carries the
	// connect deadline; everything after it carries the send deadline.
	if err := conn.SetDeadline(time.Now().Add(connectTimeout)); err != nil {
		return fmt.Errorf("email: %w", e.sanitizeError(err))
	}
	if err := e.converse(conn, body, sendTimeout); err != nil {
		// A cancelled caller is the reason the conversation stopped, so
		// the caller's error is what the caller gets back — not the
		// closed connection the cancellation caused.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("email: %w", ctxErr)
		}
		return err
	}
	return nil
}

// converse greets the server and delivers the message over the connection.
func (e *Email) converse(conn net.Conn, body []byte, sendTimeout time.Duration) error {
	client, err := smtp.NewClient(conn, e.cfg.Host)
	if err != nil {
		return fmt.Errorf("email: greeting: %w", e.sanitizeError(err))
	}
	defer func() { _ = client.Close() }()
	if err := conn.SetDeadline(time.Now().Add(sendTimeout)); err != nil {
		return fmt.Errorf("email: %w", e.sanitizeError(err))
	}
	return e.deliver(client, body)
}

// deliver runs the SMTP conversation: the optional STARTTLS upgrade, the
// optional login, the envelope and the message itself.
func (e *Email) deliver(client *smtp.Client, body []byte) error {
	// STARTTLS is opportunistic in the protocol, which is exactly the
	// problem: a relay that stops advertising it turns an encrypted
	// session into a plaintext one without anybody noticing. A channel
	// that requires TLS refuses the delivery rather than downgrade.
	if offered, _ := client.Extension("STARTTLS"); offered {
		if err := client.StartTLS(e.tlsConfig()); err != nil {
			return fmt.Errorf("email: starttls: %w", e.sanitizeError(err))
		}
	} else if e.cfg.RequireTLS {
		return fmt.Errorf("email: the server does not offer STARTTLS but the channel requires TLS")
	}

	if e.cfg.Username != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fmt.Errorf("email: the server does not offer authentication but the channel is configured with a username")
		}
		if err := client.Auth(smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.Host)); err != nil {
			return fmt.Errorf("email: authentication failed: %w", e.sanitizeError(err))
		}
	}

	if err := client.Mail(e.cfg.From); err != nil {
		return fmt.Errorf("email: %w", e.sanitizeError(err))
	}

	// Every recipient is attempted even when an earlier one is refused, so
	// the operator learns how many of them the server took. A refused
	// recipient is never silently dropped: the delivery is reported as a
	// failure even when the message reached the others.
	refused := 0
	var refusal error
	for _, to := range e.cfg.To {
		err := client.Rcpt(to)
		if err == nil {
			continue
		}
		refused++
		if refusal == nil {
			refusal = e.sanitizeError(err)
		}
	}
	if refused == len(e.cfg.To) {
		return fmt.Errorf("email: the server refused every recipient: %w", refusal)
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: %w", e.sanitizeError(err))
	}
	if _, err := writer.Write(body); err != nil {
		return fmt.Errorf("email: %w", e.sanitizeError(err))
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("email: %w", e.sanitizeError(err))
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("email: %w", e.sanitizeError(err))
	}
	if refused > 0 {
		return fmt.Errorf("email: %d of %d recipients were refused by the server: %w", refused, len(e.cfg.To), refusal)
	}
	return nil
}

// sensitiveValues are the config values that must never reach an error
// message: the credentials, the addresses a server may quote back when it
// refuses a recipient, and the host the client connected to.
func (e *Email) sensitiveValues() []string {
	out := make([]string, 0, len(e.cfg.To)+4)
	out = append(out, e.cfg.Password, e.cfg.Username, e.cfg.From, e.cfg.Host)
	out = append(out, e.cfg.To...)
	if e.cfg.Port > 0 {
		out = append(out, net.JoinHostPort(e.cfg.Host, strconv.Itoa(e.cfg.Port)))
	}
	return out
}

// sanitizeError makes an error from the SMTP conversation safe to store as a
// delivery snippet. The server's own reply can quote the address it refused
// and some servers echo the username that failed to authenticate, so the
// channel's values are taken out of the message. The original error is kept
// underneath, because the reply code is what tells channel health whether
// the failure is permanent.
func (e *Email) sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	return &sanitizedError{msg: e.sanitizeText(err.Error()), err: err}
}

// sanitizeText strips a server reply of the channel's own values and of the
// line breaks that would forge a second line in a log file or a delivery
// snippet.
func (e *Email) sanitizeText(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, text)
	for _, secret := range e.sensitiveValues() {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	const maxLen = 200
	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}
	return strings.TrimSpace(text)
}

// sanitizedError prints a message with the channel's own values removed
// while keeping the original error underneath, so errors.Is and errors.As
// still see the timeout and the SMTP reply code — the code is what tells a
// permanent failure (5xx) from a transient one (4xx).
type sanitizedError struct {
	msg string
	err error
}

func (e *sanitizedError) Error() string { return e.msg }

func (e *sanitizedError) Unwrap() error { return e.err }
