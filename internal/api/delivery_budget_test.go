package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// budgetStore is a store.Store fake holding one monitor's budget.
type budgetStore struct {
	store.Store
	limit       *int
	used        int
	exhaustedAt *time.Time
	resets      int
	missing     bool
}

func (b *budgetStore) GetMonitor(context.Context, int64) (*store.Monitor, error) {
	if b.missing {
		return nil, store.ErrNotFound
	}
	return &store.Monitor{ID: 1, Name: "m"}, nil
}

func (b *budgetStore) GetDeliveryBudget(_ context.Context, id int64, now time.Time) (*store.DeliveryBudget, error) {
	return &store.DeliveryBudget{
		MonitorID: id, Limit: b.limit, Used: b.used, ExhaustedAt: b.exhaustedAt,
		WindowStart: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC),
	}, nil
}

func (b *budgetStore) SetDeliveryBudget(_ context.Context, _ int64, limit *int) error {
	b.limit = limit
	return nil
}

func (b *budgetStore) ResetDeliveryBudget(context.Context, int64, time.Time) error {
	b.resets++
	b.used, b.exhaustedAt = 0, nil
	return nil
}

func TestGetDeliveryBudgetUnlimitedByDefault(t *testing.T) {
	srv := escalationServer(t, &budgetStore{})
	status, body := doRequest(t, http.MethodGet, srv.URL+"/monitors/1/delivery-budget", "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d (%v), want 200", status, body)
	}
	if body["limit"] != nil || body["remaining"] != nil || body["exhausted"] != false {
		t.Fatalf("default budget = %v, want unlimited (null limit and remaining)", body)
	}
}

func TestPutDeliveryBudgetSetsAndReportsRemaining(t *testing.T) {
	st := &budgetStore{used: 4}
	srv := escalationServer(t, st)
	status, body := doRequest(t, http.MethodPut, srv.URL+"/monitors/1/delivery-budget", `{"limit":10}`)
	if status != http.StatusOK {
		t.Fatalf("PUT = %d (%v), want 200", status, body)
	}
	if st.limit == nil || *st.limit != 10 {
		t.Fatalf("stored limit = %v, want 10", st.limit)
	}
	if body["remaining"].(float64) != 6 || body["used"].(float64) != 4 {
		t.Fatalf("body = %v, want used 4 remaining 6", body)
	}
	if body["resets_at"] == nil {
		t.Fatalf("a limited budget must say when it resets: %v", body)
	}
}

func TestPutDeliveryBudgetNullClears(t *testing.T) {
	ten := 10
	st := &budgetStore{limit: &ten}
	srv := escalationServer(t, st)
	status, _ := doRequest(t, http.MethodPut, srv.URL+"/monitors/1/delivery-budget", `{"limit":null}`)
	if status != http.StatusOK || st.limit != nil {
		t.Fatalf("PUT null = %d, limit %v; want 200 and cleared", status, st.limit)
	}
}

func TestPutDeliveryBudgetRejectsBadBodies(t *testing.T) {
	for name, body := range map[string]string{
		"missing limit": `{}`,
		"zero":          `{"limit":0}`,
		"negative":      `{"limit":-3}`,
		"wrong type":    `{"limit":"lots"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ten := 10
			st := &budgetStore{limit: &ten}
			srv := escalationServer(t, st)
			status, _ := doRequest(t, http.MethodPut, srv.URL+"/monitors/1/delivery-budget", body)
			if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
				t.Fatalf("PUT %s = %d, want a 4xx validation error", body, status)
			}
			if st.limit == nil || *st.limit != 10 {
				t.Fatalf("a rejected write must leave the budget alone, limit = %v", st.limit)
			}
		})
	}
}

func TestDeliveryBudgetResetAndUnknownMonitor(t *testing.T) {
	two := 2
	now := time.Now()
	st := &budgetStore{limit: &two, used: 2, exhaustedAt: &now}
	srv := escalationServer(t, st)
	status, body := doRequest(t, http.MethodPost, srv.URL+"/monitors/1/delivery-budget/reset", "")
	if status != http.StatusOK || st.resets != 1 {
		t.Fatalf("reset = %d, resets %d; want 200 and one reset", status, st.resets)
	}
	if body["exhausted"] != false || body["remaining"].(float64) != 2 {
		t.Fatalf("after reset = %v, want not exhausted with 2 remaining", body)
	}

	gone := escalationServer(t, &budgetStore{missing: true})
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/monitors/1/delivery-budget"},
		{http.MethodPut, "/monitors/1/delivery-budget"},
		{http.MethodPost, "/monitors/1/delivery-budget/reset"},
	} {
		status, _ := doRequest(t, c.method, gone.URL+c.path, `{"limit":1}`)
		if status != http.StatusNotFound {
			t.Errorf("%s %s on unknown monitor = %d, want 404", c.method, c.path, status)
		}
	}
}
