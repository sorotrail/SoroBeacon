package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
