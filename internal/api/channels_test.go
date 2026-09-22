package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// channelsStore records the enabledOnly flag ListChannels was called with,
// and returns a fixed set of channels regardless of it — the filtering
// itself is the store's job (tested against Postgres); the handler's job
// is wiring the query param through correctly.
type channelsStore struct {
	store.Store
	gotEnabledOnly *bool
	gotType        string
}

func (c *channelsStore) ListChannelsPage(ctx context.Context, f store.ListFilter) ([]store.Channel, error) {
	c.gotEnabledOnly = &f.EnabledOnly
	c.gotType = f.Type
	return []store.Channel{{ID: 1, Name: "ops", Type: "webhook", Enabled: true}}, nil
}

func TestListChannels_EnabledParamWiring(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"no param", "", false},
		{"enabled=true", "?enabled=true", true},
		{"enabled=false leaves default behavior unchanged", "?enabled=false", false},
		{"enabled=garbage leaves default behavior unchanged", "?enabled=garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := &channelsStore{}
			srv := httptest.NewServer(newProbeServer(cs, &fakeRPC{}))
			defer srv.Close()

			res, err := http.Get(srv.URL + "/channels" + tt.query)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET /channels%s = %d, want 200", tt.query, res.StatusCode)
			}
			if cs.gotEnabledOnly == nil {
				t.Fatal("ListChannels was not called")
			}
			if *cs.gotEnabledOnly != tt.want {
				t.Fatalf("enabledOnly = %v, want %v", *cs.gotEnabledOnly, tt.want)
			}

			var body map[string]any
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			list, _ := body["channels"].([]any)
			if len(list) != 1 {
				t.Fatalf("got %d channels, want 1 (body=%v)", len(list), body)
			}
		})
	}
}

// TestListChannels_TypeFilter covers the ?type= filter. The filter itself is
// applied in SQL; what the handler owns is validating the value and passing
// it through, so an unknown type is a 400 rather than an empty list that
// reads like "no channels configured".
func TestListChannels_TypeFilter(t *testing.T) {
	t.Run("passes a known type through", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels?type=webhook")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if cs.gotType != "webhook" {
			t.Fatalf("type = %q, want %q", cs.gotType, "webhook")
		}
	})

	t.Run("rejects an unknown type", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels?type=carrier-pigeon")
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})

	t.Run("no type means no filter", func(t *testing.T) {
		cs := &channelsStore{}
		code, _ := getJSON(t, cs, "/channels")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if cs.gotType != "" {
			t.Fatalf("type = %q, want empty", cs.gotType)
		}
	})
}
