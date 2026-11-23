package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// A federated alert belongs to no local monitor and no local rule, so the
// detail page has to say where it came from instead of printing the zero
// ids it does not have.
func TestAlertDetailPageNamesTheOriginOfAFederatedAlert(t *testing.T) {
	st := alertDetailStore{
		alert: store.Alert{
			ID:               42,
			EventID:          "evt-abc",
			Federated:        true,
			FederationOrigin: "peer-a",
			SourceMonitorID:  3,
			SourceRuleID:     9,
			CreatedAt:        time.Date(2026, 9, 21, 12, 1, 0, 0, time.UTC),
			Payload:          json.RawMessage(`{"contract_id":"CA7QYNF7"}`),
		},
	}
	s, err := New(st, rules.NewRegistry(), notify.DefaultFactory(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/alerts/42")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /alerts/42 = %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, "peer-a") {
		t.Errorf("the page must name the instance the alert came from:\n%s", html)
	}
	if strings.Contains(html, ">#0<") {
		t.Errorf("the page must not print the zero ids it does not have:\n%s", html)
	}
}
