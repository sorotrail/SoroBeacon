package web

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

const (
	alice = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDW"
	bob   = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABCDX"
)

func wrapPayload(event string, topics []any, value any) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"event_name": event,
		"topics":     topics,
		"value":      value,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func wrapperTopics(event, from, to string) []any {
	return []any{
		map[string]any{"symbol": event},
		map[string]any{"address": from},
		map[string]any{"address": to},
	}
}

func TestDecodedFields(t *testing.T) {
	i128 := map[string]any{"i128": "1000000"}
	tests := []struct {
		name string
		in   json.RawMessage
		want []decodedField
	}{
		{
			name: "transfer wrappers",
			in:   wrapPayload("transfer", wrapperTopics("transfer", alice, bob), i128),
			want: []decodedField{
				{Label: "event", Value: "transfer"},
				{Label: "from", Value: alice},
				{Label: "to", Value: bob},
				{Label: "amount", Value: "1000000"},
			},
		},
		{
			name: "transfer bare strings",
			in:   wrapPayload("transfer", []any{"transfer", alice, bob}, i128),
			want: []decodedField{
				{Label: "event", Value: "transfer"},
				{Label: "from", Value: alice},
				{Label: "to", Value: bob},
				{Label: "amount", Value: "1000000"},
			},
		},
		{
			name: "mint",
			in:   wrapPayload("mint", wrapperTopics("mint", alice, bob), i128),
			want: []decodedField{
				{Label: "event", Value: "mint"},
				{Label: "from", Value: alice},
				{Label: "to", Value: bob},
				{Label: "amount", Value: "1000000"},
			},
		},
		{
			name: "burn from is the holder",
			in:   wrapPayload("burn", wrapperTopics("burn", alice, bob), map[string]any{"i128": "500"}),
			want: []decodedField{
				{Label: "event", Value: "burn"},
				{Label: "from", Value: bob},
				{Label: "to", Value: alice},
				{Label: "amount", Value: "500"},
			},
		},
		{
			name: "clawback from is the holder",
			in:   wrapPayload("clawback", wrapperTopics("clawback", alice, bob), i128),
			want: []decodedField{
				{Label: "event", Value: "clawback"},
				{Label: "from", Value: bob},
				{Label: "to", Value: alice},
				{Label: "amount", Value: "1000000"},
			},
		},
		{
			name: "set_admin has no amount",
			in:   wrapPayload("set_admin", wrapperTopics("set_admin", alice, bob), nil),
			want: []decodedField{
				{Label: "event", Value: "set_admin"},
				{Label: "from", Value: alice},
				{Label: "to", Value: bob},
			},
		},
		{
			name: "unknown event numbered topics",
			in: wrapPayload("custom", []any{
				map[string]any{"symbol": "custom"},
				map[string]any{"address": alice},
				"bare",
			}, map[string]any{"string": "hello"}),
			want: []decodedField{
				{Label: "event", Value: "custom"},
				{Label: "0", Value: "custom"},
				{Label: "1", Value: alice},
				{Label: "2", Value: "bare"},
				{Label: "value", Value: "hello"},
			},
		},
		{
			name: "empty",
			in:   json.RawMessage(nil),
			want: nil,
		},
		{
			name: "invalid json",
			in:   json.RawMessage("not json"),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodedFields(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("len=%d want %d\ngot %#v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("field %d = %#v, want %#v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestDecodedEventEscapes(t *testing.T) {
	raw := wrapPayload("custom", []any{map[string]any{"symbol": "<script>"}}, "<b>")
	htmlOut := string(decodedEvent(raw))
	if strings.Contains(htmlOut, "<script>") || strings.Contains(htmlOut, "<b>") {
		t.Fatalf("unescaped markup leaked: %s", htmlOut)
	}
	if !strings.Contains(htmlOut, html.EscapeString("<script>")) {
		t.Fatalf("expected escaped symbol, got %s", htmlOut)
	}
}

func TestAlertsPageRendersDecodedAndRaw(t *testing.T) {
	payload := wrapPayload("transfer", wrapperTopics("transfer", alice, bob), map[string]any{"i128": "42"})
	s, err := New(payloadStore{payload: payload}, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/alerts")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	if !strings.Contains(page, `class="decoded"`) {
		t.Fatalf("missing decoded table:\n%s", page)
	}
	if !strings.Contains(page, alice) || !strings.Contains(page, ">from<") {
		t.Fatalf("missing labelled from slot:\n%s", page)
	}
	if !strings.Contains(page, "<pre>") {
		t.Fatalf("raw JSON pre missing:\n%s", page)
	}
}

// payloadStore lets tests inject a single alert payload into /alerts.
type payloadStore struct {
	emptyStore
	payload json.RawMessage
}

func (p payloadStore) ListAlerts(context.Context, store.AlertFilter) ([]store.Alert, error) {
	return []store.Alert{{ID: 1, Payload: p.payload}}, nil
}
