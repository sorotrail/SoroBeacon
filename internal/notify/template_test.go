package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textChannels is every channel type that renders the shared plain-text
// message. The generic webhook sends structured JSON instead and has no
// template option, so it is deliberately absent.
var textChannels = []string{TypeDiscord, TypeSlack, TypeTelegram, TypeEmail}

// mustJSON marshals a config map for a channel constructor.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// buildTextChannel builds the named channel with an optional template.
// endpoint is used by the webhook-style channels and ignored by email.
func buildTextChannel(t *testing.T, name, tpl, endpoint string) (Notifier, error) {
	t.Helper()
	switch name {
	case TypeDiscord:
		return NewDiscord(mustJSON(t, map[string]string{"webhook_url": endpoint, "template": tpl}))
	case TypeSlack:
		return NewSlack(mustJSON(t, map[string]string{"webhook_url": endpoint, "template": tpl}))
	case TypeTelegram:
		return NewTelegram(mustJSON(t, map[string]string{"bot_token": "t", "chat_id": "c", "api_base": endpoint, "template": tpl}))
	case TypeEmail:
		return NewEmail(mustJSON(t, map[string]any{
			"host": "smtp.example.test", "from": "beacon@example.com",
			"to": []string{"ops@example.com"}, "template": tpl,
		}))
	default:
		t.Fatalf("unknown text channel %q", name)
		return nil, nil
	}
}

// deliveredText builds the named channel with tpl and returns the message it
// actually delivers: the POSTed text for the chat channels, the message body
// for email (whose subject is built separately).
func deliveredText(t *testing.T, name, tpl string, alert Alert) string {
	t.Helper()

	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	t.Cleanup(srv.Close)

	n, err := buildTextChannel(t, name, tpl, srv.URL)
	require.NoError(t, err)

	if e, ok := n.(*Email); ok {
		body, err := e.body(alert)
		require.NoError(t, err)
		// Headers and body are separated by a blank line.
		_, msg, _ := strings.Cut(string(body), "\r\n\r\n")
		return msg
	}

	require.NoError(t, n.Send(context.Background(), alert))
	if name == TypeDiscord {
		return got["content"]
	}
	return got["text"]
}

// TestCustomTemplateRendersForEveryTextChannel is the core requirement: a
// custom template must take effect for every channel that uses RenderText,
// not just one of them.
func TestCustomTemplateRendersForEveryTextChannel(t *testing.T) {
	const tpl = "CUSTOM {{.MonitorName}} saw {{.EventName}}"
	alert := Alert{MonitorName: "Treasury", RuleType: "value_threshold", EventName: "transfer"}

	for _, name := range textChannels {
		t.Run(name, func(t *testing.T) {
			got := deliveredText(t, name, tpl, alert)
			assert.Contains(t, got, "CUSTOM Treasury saw transfer")
			assert.NotContains(t, got, "SoroBeacon alert", "the custom template must replace the default message")
		})
	}
}

// TestUnsetTemplateUsesDefaultMessage proves the override is strictly
// additive: without a template (or with a blank one) the message is exactly
// what every channel sent before the option existed.
func TestUnsetTemplateUsesDefaultMessage(t *testing.T) {
	alert := Alert{MonitorName: "Treasury", RuleType: "value_threshold", EventID: "e1"}
	want, err := RenderText(alert)
	require.NoError(t, err)

	for _, name := range textChannels {
		for _, tpl := range []string{"", "   "} {
			t.Run(name+"/"+fmt.Sprintf("%q", tpl), func(t *testing.T) {
				assert.Equal(t, want, deliveredText(t, name, tpl, alert))
			})
		}
	}
}

// TestInvalidTemplateRejectedAtConstruction covers the validation half of the
// contract: a syntax error is rejected when the channel is built (so the API
// answers 400 on create/update), not discovered at delivery time.
func TestInvalidTemplateRejectedAtConstruction(t *testing.T) {
	const bad = "{{.MonitorName" // unclosed action: a parse error
	for _, name := range textChannels {
		t.Run(name, func(t *testing.T) {
			_, err := buildTextChannel(t, name, bad, "https://example.test/hook")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "template")
			assert.Contains(t, err.Error(), name, "the error must name the channel type")
		})
	}
}

// TestTemplateExecutionErrorFallsBackToDefault covers the other half: a
// template that parses but fails at execution (here, an unknown field) must
// fall back to the default message rather than drop the alert.
func TestTemplateExecutionErrorFallsBackToDefault(t *testing.T) {
	const tpl = "{{.NoSuchField}}"
	alert := Alert{MonitorName: "Treasury", EventID: "e1"}
	want, err := RenderText(alert)
	require.NoError(t, err)

	for _, name := range textChannels {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, deliveredText(t, name, tpl, alert))
		})
	}
}

// TestChannelTemplateRenderFallsBackOnExecutionError exercises the shared
// helper directly with a template that parses but indexes past the end of a
// field, and asserts the operator-visible warning is logged.
func TestChannelTemplateRenderFallsBackOnExecutionError(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tpl, err := parseChannelTemplate("{{index .Payload 99}}")
	require.NoError(t, err, "a bad index is a parse-time success")

	alert := Alert{MonitorName: "m"}
	got, err := tpl.render(alert)
	require.NoError(t, err)
	want, err := RenderText(alert)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Contains(t, logs.String(), "channel template failed to execute")
}

// TestTemplateCanAddressEveryAlertField pins the template data to the Alert
// struct: the fields documented for operators in docs/channels/templates.md
// are exactly the exported Alert fields, and a template may use all of them.
func TestTemplateCanAddressEveryAlertField(t *testing.T) {
	var b strings.Builder
	rt := reflect.TypeOf(Alert{})
	for i := 0; i < rt.NumField(); i++ {
		fmt.Fprintf(&b, "{{.%s}}\n", rt.Field(i).Name)
	}
	tpl, err := parseChannelTemplate(b.String())
	require.NoError(t, err)
	_, err = tpl.render(Alert{})
	require.NoError(t, err, "every exported Alert field must be addressable from a template")
}

// NewPerMonitorTemplate is not referenced by any channel constructor or by
// the Factory — per-monitor overrides are not wired into delivery at all. The
// tests below therefore pin the standalone behaviour the function does have,
// and the PR records the missing wiring as a finding rather than pretending
// the feature exists.
func TestPerMonitorTemplateRendersAlert(t *testing.T) {
	tpl := NewPerMonitorTemplate("MON {{.MonitorName}} {{.EventName}} @{{.Ledger}}")
	require.NotNil(t, tpl)

	var b strings.Builder
	require.NoError(t, tpl.tmpl.Execute(&b, Alert{MonitorName: "Treasury", EventName: "transfer", Ledger: 42}))
	assert.Equal(t, "MON Treasury transfer @42", b.String())
}

// TestPerMonitorTemplatePanicsOnMalformedPattern pins a contrast with
// parseChannelTemplate: malformed input panics here (template.Must) instead of
// returning an error the API can turn into a 400. Today nothing calls this
// function, so no request can trigger the panic; the finding is that wiring
// it up as-is would turn an operator typo into a crash.
func TestPerMonitorTemplatePanicsOnMalformedPattern(t *testing.T) {
	assert.Panics(t, func() {
		NewPerMonitorTemplate("{{.MonitorName")
	})
}

// TestPerMonitorTemplateExecutionErrorHasNoFallback is the other half of the
// contrast. channelTemplate.render logs and falls back to the default message
// when execution fails; PerMonitorTemplate exposes no such path, so a template
// that parses but references a missing field simply cannot produce a message.
func TestPerMonitorTemplateExecutionErrorHasNoFallback(t *testing.T) {
	tpl := NewPerMonitorTemplate("{{.NoSuchField}}")

	var b strings.Builder
	err := tpl.tmpl.Execute(&b, Alert{MonitorName: "Treasury"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NoSuchField")
}

// TestTemplateDataCannotAddressChannelConfig checks the credential-disclosure
// path the other way around: templates execute against Alert, which carries
// no channel config, and neither template implementation can address one.
// Alert.Payload and Alert.Enrichment are operator data, not config.
func TestTemplateDataCannotAddressChannelConfig(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rt := reflect.TypeOf(Alert{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		for _, forbidden := range []string{"config", "secret", "token", "password", "webhook", "credential"} {
			assert.NotContains(t, name, forbidden,
				"template data must not expose anything config-like: Alert.%s", rt.Field(i).Name)
		}
	}

	channelTpl, err := parseChannelTemplate("{{.Config}}")
	require.NoError(t, err)
	full, err := channelTpl.render(Alert{})
	require.NoError(t, err, "render falls back rather than failing the alert")
	defaultMessage, err := RenderText(Alert{})
	require.NoError(t, err)
	assert.Equal(t, defaultMessage, full, "a template cannot reach channel config and falls back")

	perMonitor := NewPerMonitorTemplate("{{.Config}}")
	var b strings.Builder
	require.Error(t, perMonitor.tmpl.Execute(&b, Alert{}),
		"Alert has no Config field, so a config-interpolating template cannot resolve")
}

// TestTemplateOutputCannotBreakChatPayload covers the realistic failure mode
// for a plain-text template: an event name containing quotes, backslashes and
// newlines must still produce a valid JSON request body for every chat
// channel. text/template inserts values verbatim; the channels' JSON encoder
// is what keeps the payload intact.
func TestTemplateOutputCannotBreakChatPayload(t *testing.T) {
	const hostile = "quote \" backslash \\ newline\nclosing }"

	for _, name := range []string{TypeDiscord, TypeSlack, TypeTelegram} {
		t.Run(name, func(t *testing.T) {
			bodies := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				bodies <- body
			}))
			t.Cleanup(srv.Close)

			n, err := buildTextChannel(t, name, "{{.EventName}}", srv.URL)
			require.NoError(t, err)
			require.NoError(t, n.Send(context.Background(), Alert{EventName: hostile}))

			var raw []byte
			select {
			case raw = <-bodies:
			case <-time.After(2 * time.Second):
				t.Fatal("channel made no request")
			}
			require.True(t, json.Valid(raw), "template output broke the JSON payload: %s", raw)

			var payload map[string]string
			require.NoError(t, json.Unmarshal(raw, &payload))
			field := "text"
			if name == TypeDiscord {
				field = "content"
			}
			assert.Equal(t, hostile, payload[field],
				"template values must round-trip unchanged through the channel payload")
		})
	}
}
