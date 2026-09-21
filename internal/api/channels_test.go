package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// channelsStore records the ChannelFilter ListChannels was called with.
// Filtering itself is the store's job (tested against Postgres); the
// handler's job is wiring query params through correctly and never
// turning an unknown type into a 400.
type channelsStore struct {
	store.Store
	got         *store.ChannelFilter
	returnEmpty bool
	returnList  []store.Channel
}

func (c *channelsStore) ListChannels(ctx context.Context, f store.ChannelFilter) ([]store.Channel, error) {
	got := f
	c.got = &got
	if c.returnEmpty {
		return []store.Channel{}, nil
	}
	if c.returnList != nil {
		return c.returnList, nil
	}
	return []store.Channel{{ID: 1, Name: "ops", Type: "webhook", Enabled: true}}, nil
}

func TestListChannels_QueryParamWiring(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  store.ChannelFilter
	}{
		{"no param", "", store.ChannelFilter{}},
		{"enabled=true", "?enabled=true", store.ChannelFilter{EnabledOnly: true}},
		{"enabled=false leaves default behavior unchanged", "?enabled=false", store.ChannelFilter{}},
		{"enabled=garbage leaves default behavior unchanged", "?enabled=garbage", store.ChannelFilter{}},
		{"type=slack", "?type=slack", store.ChannelFilter{Type: "slack"}},
		{"type composes with enabled", "?type=slack&enabled=true", store.ChannelFilter{EnabledOnly: true, Type: "slack"}},
		{"empty type is omitted", "?type=", store.ChannelFilter{}},
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
			if cs.got == nil {
				t.Fatal("ListChannels was not called")
			}
			if *cs.got != tt.want {
				t.Fatalf("filter = %+v, want %+v", *cs.got, tt.want)
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

func TestListChannels_UnknownTypeIsEmptyList(t *testing.T) {
	cs := &channelsStore{returnEmpty: true}
	srv := httptest.NewServer(newProbeServer(cs, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/channels?type=not-a-real-type")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unknown type = %d, want 200 (not 400)", res.StatusCode)
	}
	if cs.got == nil {
		t.Fatal("ListChannels was not called")
	}
	if cs.got.Type != "not-a-real-type" {
		t.Fatalf("type = %q, want not-a-real-type", cs.got.Type)
	}

	var got []store.Channel
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("decoded nil slice; want empty JSON array")
	}
	if len(got) != 0 {
		t.Fatalf("got %d channels, want 0", len(got))
	}
}
