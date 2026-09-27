package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type dailyStore struct {
	store.Store
	days []store.AlertDayCount
	got  int
}

func (d *dailyStore) AlertCountsByDay(_ context.Context, days int) ([]store.AlertDayCount, error) {
	d.got = days
	return d.days, nil
}

func TestAlertsDailyReturnsUTCSeries(t *testing.T) {
	st := &dailyStore{days: []store.AlertDayCount{
		{Day: "2026-09-01", Count: 0},
		{Day: "2026-09-02", Count: 4},
	}}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/stats/alerts-daily")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /stats/alerts-daily = %d, want 200", res.StatusCode)
	}
	if st.got != store.AlertSeriesDays {
		t.Fatalf("AlertCountsByDay days = %d, want %d", st.got, store.AlertSeriesDays)
	}
	var body alertsDailyResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want UTC", body.Timezone)
	}
	if len(body.Days) != 2 || body.Days[1].Count != 4 {
		t.Fatalf("days = %+v", body.Days)
	}
}

func TestAlertsDailyNilSeriesIsEmptyArray(t *testing.T) {
	st := &dailyStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/stats/alerts-daily")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	days, ok := body["days"].([]any)
	if !ok || days == nil {
		t.Fatalf("days = %#v, want []", body["days"])
	}
	if len(days) != 0 {
		t.Fatalf("days len = %d, want 0", len(days))
	}
}
