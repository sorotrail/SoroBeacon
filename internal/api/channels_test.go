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
}

func (c *channelsStore) ListChannels(ctx context.Context, enabledOnly bool) ([]store.Channel, error) {
	c.gotEnabledOnly = &enabledOnly
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

			var got []store.Channel
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d channels, want 1", len(got))
			}
		})
	}
}
