package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// teamsTestAlert is the fixed alert every Teams test delivers, so the
// assertions on the posted card are about the card, not the input.
var teamsTestAlert = Alert{
	ID:          7,
	MonitorName: "treasury",
	RuleID:      3,
	RuleType:    "value_threshold",
	EventID:     "0000015985000000000-00001",
	ContractID:  "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUWDA",
	EventName:   "transfer",
	Ledger:      123456,
	TxHash:      "abc123",
	CreatedAt:   time.Date(2026, 7, 21, 9, 41, 32, 0, time.UTC),
	Severity:    "critical",
}

func TestTeamsConfigValidation(t *testing.T) {
	// Missing webhook_url.
	_, err := NewTeams(json.RawMessage(`{}`))
	assert.Error(t, err)

	// Non-HTTPS URL.
	_, err = NewTeams(json.RawMessage(`{"webhook_url": "http://example.com/hook"}`))
	assert.Error(t, err, "webhook_url must be rejected unless HTTPS")

	// Malformed JSON.
	_, err = NewTeams(json.RawMessage(`not json`))
	assert.Error(t, err)

	// A valid HTTPS URL is accepted.
	_, err = NewTeams(json.RawMessage(`{"webhook_url": "https://example.com/hook"}`))
	require.NoError(t, err)
}

func TestTeamsSendPostsMessageCard(t *testing.T) {
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	n, err := NewTeams(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)
	require.NoError(t, n.Send(context.Background(), teamsTestAlert))

	assert.Contains(t, gotCT, "application/json")

	var card teamsMessageCard
	require.NoError(t, json.Unmarshal(gotBody, &card))
	assert.Equal(t, "MessageCard", card.Type)
	assert.Equal(t, "http://schema.org/extensions", card.Context)
	assert.Contains(t, card.Title, "treasury", "the monitor name must be readable at a glance")
	assert.Equal(t, "FF0000", card.ThemeColor, "critical severity maps to the red accent")
	require.Len(t, card.Sections, 1)
	section := card.Sections[0]
	assert.Contains(t, section.Text, "transfer", "the message body carries the alert text")

	// Rule type, event ID and timestamp must be present as facts.
	facts := map[string]string{}
	for _, f := range section.Facts {
		facts[f.Name] = f.Value
	}
	assert.Contains(t, facts["Rule"], "value_threshold")
	assert.Equal(t, teamsTestAlert.EventID, facts["Event ID"])
	assert.Equal(t, "2026-07-21 09:41:32 UTC", facts["At"])
	assert.Equal(t, "treasury", facts["Monitor"])
}

func TestTeamsSendsDigestVerbatim(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	n, err := NewTeams(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)

	digest := "SoroBeacon digest: 3 alert(s) across 1 monitor(s)"
	require.NoError(t, n.Send(context.Background(), Alert{ID: 1, Digest: digest}))

	var card teamsMessageCard
	require.NoError(t, json.Unmarshal(gotBody, &card))
	require.Len(t, card.Sections, 1)
	assert.Equal(t, digest, card.Sections[0].Text, "a digest summary is sent verbatim")
}

func TestTeamsNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "webhook not found", http.StatusNotFound)
	}))
	defer srv.Close()

	n, err := NewTeams(json.RawMessage(fmt.Sprintf(`{"webhook_url": %q}`, srv.URL)))
	require.NoError(t, err)

	err = n.Send(context.Background(), teamsTestAlert)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404", "the error carries the status code")
	assert.NotContains(t, err.Error(), srv.URL, "errors must not leak the webhook URL")
}

func TestTeamsRegisteredInDefaultFactory(t *testing.T) {
	assert.Contains(t, DefaultFactory().Types(), TypeTeams)
}
