package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailSubjectPrefix(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "no prefix configured",
			config: `{"host":"smtp.example.com","from":"beacon@example.com","to":["ops@example.com"]}`,
			want:   "SoroBeacon alert: my-monitor",
		},
		{
			name:   "prefix configured",
			config: `{"host":"smtp.example.com","from":"beacon@example.com","to":["ops@example.com"],"subject_prefix":"[PROD] "}`,
			want:   "[PROD] SoroBeacon alert: my-monitor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewEmail(json.RawMessage(tt.config))
			require.NoError(t, err)
			e, ok := n.(*Email)
			require.True(t, ok)

			got := e.subject(Alert{MonitorName: "my-monitor"})
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEmailCheckAuthenticatesWithoutSending(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{authMechanisms: []string{"PLAIN"}})
	n, err := NewEmail(json.RawMessage(fmt.Sprintf(
		`{"host":%q,"port":%d,"username":"beacon","password":"secret","from":"beacon@example.test","to":["ops@example.test"]}`,
		srv.host(), srv.port(),
	)))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, n.(PreflightChecker).Check(ctx))

	session := srv.waitSession(t)
	assert.True(t, session.authSeen)
	assert.Equal(t, "beacon", session.authUser)
	assert.Equal(t, "secret", session.authPass)
	assert.Empty(t, session.mailFrom)
	assert.Empty(t, session.rcptTo)
	assert.Empty(t, session.data)
	assert.Contains(t, session.commands, "QUIT")
}

// smtpTestOptions configure the in-process SMTP server the email channel's
// send path is driven against.
//
// The server advertises exactly what a test asks it to, because the TLS
// cases depend on what the relay offers: a client that requires TLS has to
// refuse a server that stays silent about STARTTLS.
type smtpTestOptions struct {
	// advertiseSTARTTLS adds the STARTTLS extension to the EHLO reply.
	advertiseSTARTTLS bool
	// tlsConfig is the certificate the server presents after STARTTLS.
	tlsConfig *tls.Config
	// authMechanisms are advertised in the EHLO reply. Empty means the
	// server offers no authentication at all.
	authMechanisms []string
	// authReply is the code the server answers an AUTH command with: 235
	// accepts the login, 535 refuses it. Zero means 235.
	authReply int
	// authReplyText is the text of the AUTH reply. A test can make the
	// server echo the credentials back and prove the client does not
	// repeat them.
	authReplyText string
	// mailReply overrides the reply code for MAIL FROM.
	mailReply int
	// rcptRejects refuses individual recipients with the given code.
	rcptRejects map[string]int
	// stallGreeting keeps the server from sending its 220 greeting.
	stallGreeting bool
	// stallAfter stops the server replying once it has seen a command
	// starting with this prefix ("MAIL", say), so the client waits for an
	// answer that never comes.
	stallAfter string
}

// smtpTestSession is what one connection to the test server did.
type smtpTestSession struct {
	commands []string
	mailFrom string
	rcptTo   []string
	data     string
	tls      bool
	authSeen bool
	authUser string
	authPass string
}

// smtpTestServer is a minimal SMTP server, in process, speaking just enough
// of RFC 5321 for the cases the email channel has to get right: a delivered
// message, a TLS requirement the server does not meet, a rejected login, a
// refused recipient, a stalled server and the reply codes that decide
// whether a failure is permanent.
//
// No mail leaves the machine: the server is a net.Listener on the loopback
// interface and there is no SMTP server anywhere else in the test.
type smtpTestServer struct {
	t        *testing.T
	ln       net.Listener
	opts     smtpTestOptions
	sessions chan smtpTestSession
}

// newSMTPTestServer starts the server on a loopback port and stops it when
// the test ends.
func newSMTPTestServer(t *testing.T, opts smtpTestOptions) *smtpTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &smtpTestServer{
		t:        t,
		ln:       ln,
		opts:     opts,
		sessions: make(chan smtpTestSession, 8),
	}
	go srv.acceptLoop()
	t.Cleanup(func() { ln.Close() })
	return srv
}

func (s *smtpTestServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

// host is the address the channel is configured with.
func (s *smtpTestServer) host() string {
	host, _, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		return "127.0.0.1"
	}
	return host
}

// port is the port the channel is configured with.
func (s *smtpTestServer) port() int {
	_, port, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		return 0
	}
	var p int
	fmt.Sscanf(port, "%d", &p)
	return p
}

// waitSession waits for a connection to finish and returns what it did. The
// wait is bounded so a regression fails the test instead of hanging it.
func (s *smtpTestServer) waitSession(t *testing.T) smtpTestSession {
	t.Helper()
	select {
	case sess := <-s.sessions:
		return sess
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the SMTP test server to finish a session")
		return smtpTestSession{}
	}
}

func (s *smtpTestServer) handle(conn net.Conn) {
	defer conn.Close()
	sess := smtpTestSession{}
	defer func() {
		select {
		case s.sessions <- sess:
		default:
		}
	}()

	if s.opts.stallGreeting {
		// Accept the connection and say nothing. The client has to give
		// up on its own rather than wait for an answer that never comes.
		io.Copy(io.Discard, conn)
		return
	}

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	writeSMTPLine(rw, "220 testsrv ESMTP ready")
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		sess.commands = append(sess.commands, line)
		upper := strings.ToUpper(line)

		if s.opts.stallAfter != "" && strings.HasPrefix(upper, s.opts.stallAfter) {
			io.Copy(io.Discard, conn)
			return
		}

		switch {
		case strings.HasPrefix(upper, "EHLO"):
			writeSMTPLine(rw, s.ehloReply())
		case strings.HasPrefix(upper, "HELO"):
			writeSMTPLine(rw, "250 testsrv")
		case upper == "STARTTLS":
			writeSMTPLine(rw, "220 2.0.0 Ready to start TLS")
			tlsConn := tls.Server(conn, s.opts.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			sess.tls = true
			rw = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		case strings.HasPrefix(upper, "AUTH"):
			s.handleAuth(rw, &sess, line)
		case strings.HasPrefix(upper, "MAIL FROM"):
			sess.mailFrom = smtpPathArgument(line, "MAIL FROM:")
			if s.opts.mailReply != 0 {
				writeSMTPLine(rw, fmt.Sprintf("%d 5.1.0 rejected by the test server", s.opts.mailReply))
			} else {
				writeSMTPLine(rw, "250 2.1.0 Ok")
			}
		case strings.HasPrefix(upper, "RCPT TO"):
			to := smtpPathArgument(line, "RCPT TO:")
			sess.rcptTo = append(sess.rcptTo, to)
			if code, refused := s.opts.rcptRejects[to]; refused {
				writeSMTPLine(rw, fmt.Sprintf("%d 5.1.1 <%s>: Recipient address rejected", code, to))
			} else {
				writeSMTPLine(rw, "250 2.1.5 Ok")
			}
		case upper == "DATA":
			writeSMTPLine(rw, "354 End data with <CR><LF>.<CR><LF>")
			data, err := readSMTPData(rw)
			if err != nil {
				return
			}
			sess.data = data
			writeSMTPLine(rw, "250 2.0.0 Ok: queued as TESTSRV1")
		case upper == "RSET":
			writeSMTPLine(rw, "250 2.0.0 Ok")
		case upper == "NOOP":
			writeSMTPLine(rw, "250 2.0.0 Ok")
		case upper == "QUIT":
			writeSMTPLine(rw, "221 2.0.0 Bye")
			return
		default:
			writeSMTPLine(rw, "500 5.5.2 Unrecognized command")
		}
	}
}

// ehloReply builds the capabilities the server advertises. A test that does
// not ask for STARTTLS gets a server that does not offer it, which is the
// case a client that requires TLS has to refuse.
func (s *smtpTestServer) ehloReply() string {
	lines := []string{"250-testsrv greets you"}
	if s.opts.advertiseSTARTTLS {
		lines = append(lines, "250-STARTTLS")
	}
	if len(s.opts.authMechanisms) > 0 {
		lines = append(lines, "250-AUTH "+strings.Join(s.opts.authMechanisms, " "))
	}
	lines = append(lines, "250 HELP")
	return strings.Join(lines, "\r\n")
}

// handleAuth answers an AUTH command. The client sends "AUTH PLAIN <base64>",
// so the credentials arrive base64-encoded in one go.
func (s *smtpTestServer) handleAuth(rw *bufio.ReadWriter, sess *smtpTestSession, line string) {
	fields := strings.Fields(line)
	payload := ""
	if len(fields) >= 3 {
		payload = fields[2]
	} else {
		writeSMTPLine(rw, "334 ")
		challenge, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		payload = strings.TrimRight(challenge, "\r\n")
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		writeSMTPLine(rw, "501 5.5.2 Cannot decode the AUTH response")
		return
	}
	parts := strings.Split(string(raw), "\x00")
	sess.authSeen = true
	if len(parts) > 1 {
		sess.authUser = parts[1]
	}
	if len(parts) > 2 {
		sess.authPass = parts[2]
	}

	code := s.opts.authReply
	if code == 0 {
		code = 235
	}
	text := s.opts.authReplyText
	if text == "" {
		if code == 235 {
			text = "2.7.0 Authentication successful"
		} else {
			text = "5.7.8 Authentication credentials invalid"
		}
	}
	writeSMTPLine(rw, fmt.Sprintf("%d %s", code, text))
}

// readSMTPData reads the message body up to the terminating dot, undoing the
// dot-stuffing the client applies.
func readSMTPData(rw *bufio.ReadWriter) (string, error) {
	var b strings.Builder
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return "", err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return b.String(), nil
		}
		b.WriteString(strings.TrimPrefix(trimmed, "."))
		b.WriteString("\r\n")
	}
}

// smtpPathArgument extracts the address from a "MAIL FROM:<addr>" or
// "RCPT TO:<addr>" command.
func smtpPathArgument(line, prefix string) string {
	rest := strings.TrimPrefix(line, prefix)
	rest = strings.TrimPrefix(rest, "<")
	if idx := strings.Index(rest, ">"); idx >= 0 {
		rest = rest[:idx]
	}
	return strings.TrimSpace(rest)
}
func writeSMTPLine(rw *bufio.ReadWriter, line string) {
	rw.WriteString(line + "\r\n")
	rw.Flush()
}

// smtpTestCertificate generates a self-signed certificate for the test
// server. The client is configured to skip verification for it, the way an
// operator would for a relay that presents a private certificate.
func smtpTestCertificate(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sorobeacon-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// emailTestConfig builds a channel config pointing at the test server. The
// extra keys override the defaults.
func emailTestConfig(t *testing.T, srv *smtpTestServer, extra map[string]any) json.RawMessage {
	t.Helper()
	cfg := map[string]any{
		"host": srv.host(),
		"port": srv.port(),
		"from": "beacon@example.com",
		"to":   []string{"ops@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	return raw
}

// smtpHeader returns the value of a header of a received message, so a test
// asserts what the server received rather than what the client meant to
// send.
func smtpHeader(data, name string) string {
	for _, line := range strings.Split(data, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	return ""
}

// smtpBody returns the body of a received message.
func smtpBody(data string) string {
	_, body, ok := strings.Cut(data, "\r\n\r\n")
	if !ok {
		return ""
	}
	return body
}

// smtpHeaderBlock returns the header section of a received message, so a
// test can assert that no header beyond the ones the channel builds is
// present.
func smtpHeaderBlock(data string) string {
	headers, _, ok := strings.Cut(data, "\r\n\r\n")
	if !ok {
		return ""
	}
	return headers
}

// smtpHeaderNames returns the field names of a received message's header
// section. A value that managed to smuggle a header of its own would show up
// here as an extra name.
func smtpHeaderNames(data string) []string {
	var names []string
	for _, line := range strings.Split(smtpHeaderBlock(data), "\r\n") {
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		names = append(names, name)
	}
	return names
}

// captureLogs redirects the default logger into a buffer for the duration of
// the test, so a test can assert that a credential never reaches a log line.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// A successful send has to reach the server as the message the operator
// would read: the recipient, the subject and the body are asserted as the
// server received them.
func TestEmailSendDeliversMessage(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{})
	n, err := NewEmail(emailTestConfig(t, srv, nil))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{
		MonitorName: "my-monitor",
		RuleType:    "value_threshold",
		ContractID:  "CDLZBEACON",
	}))

	sess := srv.waitSession(t)
	assert.Equal(t, "beacon@example.com", sess.mailFrom)
	assert.Equal(t, []string{"ops@example.com"}, sess.rcptTo)
	assert.Equal(t, "beacon@example.com", smtpHeader(sess.data, "From"))
	assert.Equal(t, "ops@example.com", smtpHeader(sess.data, "To"))
	assert.Equal(t, "SoroBeacon alert: my-monitor", smtpHeader(sess.data, "Subject"))
	assert.Contains(t, smtpBody(sess.data), "my-monitor")
	assert.Contains(t, smtpBody(sess.data), "value_threshold")
	assert.False(t, sess.tls, "a server that does not offer STARTTLS is spoken to in the clear")
}

// Every configured recipient gets its own RCPT, and the message is sent
// once.
func TestEmailSendDeliversToEveryRecipient(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"to": []string{"ops@example.com", "oncall@example.com"},
	}))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{MonitorName: "my-monitor"}))

	sess := srv.waitSession(t)
	assert.Equal(t, []string{"ops@example.com", "oncall@example.com"}, sess.rcptTo)
	assert.Equal(t, "ops@example.com, oncall@example.com", smtpHeader(sess.data, "To"))
	assert.Contains(t, sess.data, "SoroBeacon alert: my-monitor")
}

// A recipient the server refuses is a failure, not a quiet success: the
// message still goes to the recipients the server accepted, and the
// delivery reports that one address was refused.
func TestEmailSendReportsRefusedRecipient(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{
		rcptRejects: map[string]int{"gone@example.com": 550},
	})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"to": []string{"ops@example.com", "gone@example.com"},
	}))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 of 2 recipients")

	sess := srv.waitSession(t)
	assert.Equal(t, []string{"ops@example.com", "gone@example.com"}, sess.rcptTo)
	assert.Contains(t, sess.data, "SoroBeacon alert: my-monitor", "the accepted recipient still gets the alert")
}

// When every recipient is refused there is nothing to send, and the
// delivery fails with the server's reason.
func TestEmailSendFailsWhenEveryRecipientIsRefused(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{
		rcptRejects: map[string]int{"gone@example.com": 550},
	})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"to": []string{"gone@example.com"},
	}))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused every recipient")

	sess := srv.waitSession(t)
	assert.Empty(t, sess.data, "no message is sent when no recipient accepted it")
}

// The important one: a channel that requires TLS must refuse a relay that
// does not offer STARTTLS rather than continue in plaintext, where the
// credentials and the alert would cross the network in the clear.
func TestEmailSendRefusesPlaintextWhenTLSIsRequired(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"require_tls": true,
		"username":    "beacon",
		"password":    "sup3r-s3cret-passw0rd",
	}))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires TLS")
	assert.NotContains(t, err.Error(), "sup3r-s3cret-passw0rd")

	sess := srv.waitSession(t)
	assert.Empty(t, sess.mailFrom, "the delivery is refused before the envelope is sent")
	assert.Empty(t, sess.data)
	assert.False(t, sess.authSeen, "no credentials are offered to a server that cannot encrypt the session")
}

// With STARTTLS on offer the session is upgraded, and the message travels
// over the encrypted connection.
func TestEmailSendUpgradesToSTARTTLS(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{
		advertiseSTARTTLS: true,
		tlsConfig:         &tls.Config{Certificates: []tls.Certificate{*smtpTestCertificate(t)}},
	})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"require_tls":     true,
		"tls_skip_verify": true,
	}))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{MonitorName: "my-monitor"}))

	sess := srv.waitSession(t)
	assert.True(t, sess.tls, "the session is encrypted after STARTTLS")
	assert.Contains(t, sess.commands, "STARTTLS")
	assert.Equal(t, "SoroBeacon alert: my-monitor", smtpHeader(sess.data, "Subject"))
}

// A relay that advertises STARTTLS but cannot complete the handshake must
// not be downgraded to plaintext: the delivery fails instead.
func TestEmailSendFailsWhenSTARTTLSHandshakeFails(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{
		advertiseSTARTTLS: true,
		tlsConfig:         &tls.Config{Certificates: []tls.Certificate{*smtpTestCertificate(t)}},
	})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"require_tls": true,
		// Verification stays on, so the self-signed certificate the test
		// server presents is refused.
	}))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "starttls")

	sess := srv.waitSession(t)
	assert.Empty(t, sess.data, "the alert is not delivered over an unverified session")
}

// The login the channel is configured with reaches the server, and the
// server's acceptance is what the delivery reports.
func TestEmailSendAuthenticates(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{
		authMechanisms: []string{"PLAIN"},
	})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"username": "beacon",
		"password": "sup3r-s3cret-passw0rd",
	}))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{MonitorName: "my-monitor"}))

	sess := srv.waitSession(t)
	assert.True(t, sess.authSeen)
	assert.Equal(t, "beacon", sess.authUser)
	assert.Equal(t, "sup3r-s3cret-passw0rd", sess.authPass)
	assert.Contains(t, sess.data, "SoroBeacon alert: my-monitor")
}

// A rejected login is a clear failure, and it is permanent: a wrong
// password does not fix itself, so the channel must not be retried forever.
func TestEmailSendReportsRejectedLogin(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{
		authMechanisms: []string{"PLAIN"},
		authReply:      535,
	})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"username": "beacon",
		"password": "sup3r-s3cret-passw0rd",
	}))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication failed")
	assert.True(t, IsPermanent(err), "a rejected login is permanent, not transient")

	sess := srv.waitSession(t)
	assert.Empty(t, sess.data, "a refused login sends no alert")
}

// The channel config holds the SMTP password, and the rule is absolute:
// it must not appear in an error message or a log line, not even when the
// server quotes it back.
func TestEmailSendNeverLeaksCredentials(t *testing.T) {
	const password = "sup3r-s3cret-passw0rd"
	logs := captureLogs(t)

	srv := newSMTPTestServer(t, smtpTestOptions{
		authMechanisms: []string{"PLAIN"},
		authReply:      535,
		authReplyText:  "5.7.8 Authentication failed for password " + password,
	})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"username": "beacon",
		"password": password,
	}))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), password)
	assert.NotContains(t, logs.String(), password)
}

// The address the server refused is channel config too, and it must not
// travel back into the error message.
func TestEmailSendDoesNotEchoRejectedRecipient(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{
		rcptRejects: map[string]int{"ops@example.com": 550},
	})
	n, err := NewEmail(emailTestConfig(t, srv, nil))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "ops@example.com")
	assert.Contains(t, err.Error(), "550")
}

// A recipient that carries a line break would end the header it sits in
// and turn the rest of the value into headers of its own. It is refused
// when the channel is configured, not sent.
func TestEmailSendRejectsInjectedRecipient(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{})
	_, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"to": []string{"ops@example.com\r\nBcc: attacker@example.com"},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line break")
	assert.NotContains(t, err.Error(), "attacker@example.com")

	_, err = NewEmail(emailTestConfig(t, srv, map[string]any{
		"from": "beacon@example.com\r\nBcc: attacker@example.com",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line break")
}

// The subject carries the monitor name, and the name reaches the header
// from the store, so a line break in it is neutralised: the injected header
// never reaches the server.
func TestEmailSendNeutralisesInjectedSubject(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{})
	n, err := NewEmail(emailTestConfig(t, srv, nil))
	require.NoError(t, err)

	require.NoError(t, n.Send(context.Background(), Alert{
		MonitorName: "my-monitor\r\nBcc: attacker@example.com",
	}))

	sess := srv.waitSession(t)
	assert.Equal(t, []string{"From", "To", "Subject", "MIME-Version", "Content-Type"}, smtpHeaderNames(sess.data),
		"the injected value did not become a header of its own")
	assert.Equal(t, "SoroBeacon alert: my-monitor Bcc: attacker@example.com", smtpHeader(sess.data, "Subject"))
}

// A server that accepts the connection and never sends its greeting must
// not hold the delivery: the connect timeout cuts it off.
func TestEmailSendTimesOutOnStalledGreeting(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{stallGreeting: true})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"connect_timeout_seconds": 1,
		"timeout_seconds":         5,
	}))
	require.NoError(t, err)

	start := time.Now()
	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
	assert.Less(t, elapsed, 5*time.Second, "the delivery gives up on its own")
}

// The same holds for a server that greets and then stops answering: the
// overall send timeout ends the conversation.
func TestEmailSendTimesOutOnStalledConversation(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{stallAfter: "MAIL"})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"connect_timeout_seconds": 5,
		"timeout_seconds":         1,
	}))
	require.NoError(t, err)

	start := time.Now()
	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
	assert.Less(t, elapsed, 5*time.Second, "the delivery gives up on its own")
}

// A cancelled delivery stops at once instead of waiting for the timeout.
func TestEmailSendHonoursContextCancellation(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{stallGreeting: true})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"connect_timeout_seconds": 30,
		"timeout_seconds":         30,
	}))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err = n.Send(ctx, Alert{MonitorName: "my-monitor"})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 5*time.Second, "the delivery stops when the caller gives up")
}

// A permanent SMTP failure is one that will not fix itself, so the channel
// is not retried forever; a transient one is. The distinction is the reply
// code, and it is what notify.IsPermanent reads.
func TestEmailClassifiesPermanentAndTransientFailures(t *testing.T) {
	tests := []struct {
		name      string
		code      int
		permanent bool
	}{
		{name: "permanent 550", code: 550, permanent: true},
		{name: "permanent 554", code: 554, permanent: true},
		{name: "transient 450", code: 450, permanent: false},
		{name: "transient 421", code: 421, permanent: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newSMTPTestServer(t, smtpTestOptions{mailReply: tt.code})
			n, err := NewEmail(emailTestConfig(t, srv, nil))
			require.NoError(t, err)

			err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
			require.Error(t, err)
			assert.Equal(t, tt.permanent, IsPermanent(err))
		})
	}
}

// A channel with a username against a server that offers no authentication
// is a configuration mistake, and it is reported as one instead of
// delivering an anonymous alert.
func TestEmailSendReportsMissingAuthenticationSupport(t *testing.T) {
	srv := newSMTPTestServer(t, smtpTestOptions{})
	n, err := NewEmail(emailTestConfig(t, srv, map[string]any{
		"username": "beacon",
		"password": "sup3r-s3cret-passw0rd",
	}))
	require.NoError(t, err)

	err = n.Send(context.Background(), Alert{MonitorName: "my-monitor"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not offer authentication")

	sess := srv.waitSession(t)
	assert.Empty(t, sess.data)
}

// A subject prefix is channel config and lands in a header, so a line
// break in it is refused when the channel is built.
func TestEmailRejectsInjectedSubjectPrefix(t *testing.T) {
	_, err := NewEmail(json.RawMessage(`{"host":"smtp.example.com","from":"beacon@example.com","to":["ops@example.com"],"subject_prefix":"[PROD]\r\nBcc: attacker@example.com"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line break")
	assert.NotContains(t, err.Error(), "attacker@example.com")
}

// A timeout in the config is bounded: an operator cannot ask for a
// delivery that never gives up.
func TestEmailRejectsUnboundedTimeouts(t *testing.T) {
	_, err := NewEmail(json.RawMessage(`{"host":"smtp.example.com","from":"beacon@example.com","to":["ops@example.com"],"timeout_seconds":100000}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout_seconds")

	_, err = NewEmail(json.RawMessage(`{"host":"smtp.example.com","from":"beacon@example.com","to":["ops@example.com"],"connect_timeout_seconds":-1}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect_timeout_seconds")
}
