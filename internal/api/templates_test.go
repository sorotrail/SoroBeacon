package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
)

type templateStore struct {
	fakeStore
	templates []store.MonitorTemplate
	nextID    int64
	monitors  []store.Monitor
	monNextID int64
	rules     []store.Rule
	channels  map[int64][]int64
}

func (t *templateStore) CreateMonitorTemplate(_ context.Context, tmpl *store.MonitorTemplate) error {
	t.nextID++
	tmpl.ID = t.nextID
	t.templates = append(t.templates, *tmpl)
	return nil
}

func (t *templateStore) GetMonitorTemplate(_ context.Context, id int64) (*store.MonitorTemplate, error) {
	for _, tmpl := range t.templates {
		if tmpl.ID == id {
			return &tmpl, nil
		}
	}
	return nil, store.ErrNotFound
}

func (t *templateStore) ListMonitorTemplates(_ context.Context) ([]store.MonitorTemplate, error) {
	return t.templates, nil
}

func (t *templateStore) UpdateMonitorTemplate(_ context.Context, tmpl *store.MonitorTemplate) error {
	for i, existing := range t.templates {
		if existing.ID == tmpl.ID {
			t.templates[i] = *tmpl
			return nil
		}
	}
	return store.ErrNotFound
}

func (t *templateStore) DeleteMonitorTemplate(_ context.Context, id int64) error {
	for i, tmpl := range t.templates {
		if tmpl.ID == id {
			t.templates = append(t.templates[:i], t.templates[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (t *templateStore) CreateMonitor(_ context.Context, m *store.Monitor) error {
	t.monNextID++
	m.ID = t.monNextID
	t.monitors = append(t.monitors, *m)
	return nil
}

func (t *templateStore) SetMonitorChannels(_ context.Context, monitorID int64, channelIDs []int64) error {
	if t.channels == nil {
		t.channels = make(map[int64][]int64)
	}
	t.channels[monitorID] = channelIDs
	return nil
}

func (t *templateStore) CreateRule(_ context.Context, r *store.Rule) error {
	r.ID = int64(len(t.rules) + 1)
	t.rules = append(t.rules, *r)
	return nil
}

func TestCreateTemplate(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"token watcher","description":"watches token events","rules":[{"type":"token_event","params":{"event":"transfer"}}],"channel_ids":[1],"parameters":[{"name":"contract","required":true}]}`
	res, err := http.Post(srv.URL+"/templates", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	if len(st.templates) != 1 {
		t.Fatalf("expected 1 template, got %d", len(st.templates))
	}
	if st.templates[0].Name != "token watcher" {
		t.Errorf("name = %q, want token watcher", st.templates[0].Name)
	}
}

func TestCreateTemplateValidation(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/templates", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestInstantiateTemplate(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{
			{
				ID:         1,
				Name:       "token watcher",
				Rules:      []store.MonitorTemplateRule{{Type: "token_event", Params: json.RawMessage(`{"event":"transfer"}`)}},
				ChannelIDs: []int64{1},
				Parameters: []store.TemplateParameter{{Name: "contract", Required: true}},
			},
		},
		nextID: 1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"my watcher","contract_ids":["CABCDEF"],"parameters":{"contract":"CABCDEF"}}`
	res, err := http.Post(srv.URL+"/templates/1/instantiate", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	if len(st.monitors) != 1 {
		t.Fatalf("expected 1 monitor, got %d", len(st.monitors))
	}
}

func TestInstantiateTemplateMissingParam(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{
			{
				ID:         1,
				Name:       "token watcher",
				Parameters: []store.TemplateParameter{{Name: "contract", Required: true}},
			},
		},
		nextID: 1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"my watcher","contract_ids":["CABCDEF"],"parameters":{}}`
	res, err := http.Post(srv.URL+"/templates/1/instantiate", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestDeleteTemplate(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{{ID: 1, Name: "doomed"}},
		nextID:    1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/templates/1", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.StatusCode)
	}
	if len(st.templates) != 0 {
		t.Fatalf("expected 0 templates, got %d", len(st.templates))
	}
}

func TestBulkInstantiateTemplate(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{
			{
				ID:         1,
				Name:       "token watcher",
				Rules:      []store.MonitorTemplateRule{{Type: "token_event", Params: json.RawMessage(`{"event":"transfer"}`)}},
				ChannelIDs: []int64{1},
			},
		},
		nextID: 1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"instances":[{"name":"w1","contract_ids":["C1"]},{"name":"w2","contract_ids":["C2"]}]}`
	res, err := http.Post(srv.URL+"/templates/1/instantiate/bulk", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	if len(st.monitors) != 2 {
		t.Fatalf("expected 2 monitors, got %d", len(st.monitors))
	}
}
