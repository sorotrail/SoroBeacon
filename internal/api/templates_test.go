package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// errStoreDown stands in for a database that is not there.
var errStoreDown = errors.New("store down")

type templateStore struct {
	fakeStore
	templates []store.MonitorTemplate
	nextID    int64
	monitors  []store.Monitor
	monNextID int64
	rules     []store.Rule
	channels  map[int64][]int64
	// known are the channels this workspace has, so a template that attaches
	// to a channel that does not exist can be tested.
	known []store.Channel
	// createErr makes the write fail, standing in for a store error, so a
	// test can check that a failed instantiation leaves nothing behind.
	createErr error
	// listErr, getErr and updateErr stand in for a store that is down on the
	// read and write paths these tests exercise, so the handlers' error
	// handling can be pinned without a database.
	listErr   error
	getErr    error
	updateErr error
	// ws records which workspace created each template, keyed by template id,
	// so the list test can prove the handler hands the request's workspace
	// scope to the store instead of dropping it.
	ws map[int64]workspace.ID
}

func (t *templateStore) CreateMonitorTemplate(ctx context.Context, tmpl *store.MonitorTemplate) error {
	t.nextID++
	tmpl.ID = t.nextID
	if t.ws == nil {
		t.ws = map[int64]workspace.ID{}
	}
	t.ws[tmpl.ID] = requestWorkspace(ctx)
	t.templates = append(t.templates, *tmpl)
	return nil
}

func (t *templateStore) GetMonitorTemplate(_ context.Context, id int64) (*store.MonitorTemplate, error) {
	if t.getErr != nil {
		return nil, t.getErr
	}
	for i := range t.templates {
		if t.templates[i].ID == id {
			// A copy, the way a real store hands back a row: a handler that
			// mutates what it got must not be able to edit the stored value
			// before the write is accepted.
			tmpl := t.templates[i]
			return &tmpl, nil
		}
	}
	return nil, store.ErrNotFound
}

func (t *templateStore) ListMonitorTemplates(ctx context.Context) ([]store.MonitorTemplate, error) {
	if t.listErr != nil {
		return nil, t.listErr
	}
	// Mirror the real store's `WHERE workspace_id = ?` clause: a request only
	// ever sees the templates its context is scoped to. A template with no
	// recorded workspace belongs to the default one, the tenant every
	// pre-existing row sits in.
	ws := requestWorkspace(ctx)
	var out []store.MonitorTemplate
	for _, tmpl := range t.templates {
		owner := t.ws[tmpl.ID]
		if owner == "" {
			owner = workspace.Default
		}
		if owner == ws {
			out = append(out, tmpl)
		}
	}
	return out, nil
}

func (t *templateStore) UpdateMonitorTemplate(_ context.Context, tmpl *store.MonitorTemplate) error {
	if t.updateErr != nil {
		return t.updateErr
	}
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

// CreateMonitorWithRules records the monitor, its rules and its channel
// links the way the store does: one call, all of it or none of it.
func (t *templateStore) CreateMonitorWithRules(_ context.Context, m *store.Monitor, rules []*store.Rule, channelIDs []int64) error {
	if t.createErr != nil {
		return t.createErr
	}
	t.monNextID++
	m.ID = t.monNextID
	t.monitors = append(t.monitors, *m)
	for _, r := range rules {
		r.ID = int64(len(t.rules) + 1)
		r.MonitorID = m.ID
		t.rules = append(t.rules, *r)
	}
	if len(channelIDs) > 0 {
		if t.channels == nil {
			t.channels = make(map[int64][]int64)
		}
		t.channels[m.ID] = channelIDs
	}
	return nil
}

func (t *templateStore) ListChannels(_ context.Context, _ bool) ([]store.Channel, error) {
	return t.known, nil
}

// postTemplateJSON sends a JSON body to a template server and returns the
// response and its error envelope.
func postTemplateJSON(t *testing.T, url, body string) (*http.Response, errorEnvelope) {
	t.Helper()
	res, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, raw)
	}
	return res, env
}

// detailReason returns the reason the envelope gives for one field.
func detailReason(env errorEnvelope, field string) (string, bool) {
	for _, d := range env.Details {
		if d.Field == field {
			return d.Reason, true
		}
	}
	return "", false
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

// TestCreateTemplateRejectsUnknownRuleType: a template with a rule type the
// registry does not know is rejected when it is created, not silently at
// every instantiation.
func TestCreateTemplateRejectsUnknownRuleType(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := postTemplateJSON(t, srv.URL+"/templates",
		`{"name":"broken","rules":[{"type":"not_a_rule","params":{}}]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", res.StatusCode, env)
	}
	if reason, ok := detailReason(env, "rules[0].type"); !ok || reason != "unknown rule type" {
		t.Errorf("details = %+v, want rules[0].type to name the unknown rule type", env.Details)
	}
	if len(st.templates) != 0 {
		t.Errorf("a rejected template was stored: %+v", st.templates)
	}
}

// TestCreateTemplateRejectsInvalidParams: the parts of a rule's params that
// do not depend on substitution are checked when the template is written.
func TestCreateTemplateRejectsInvalidParams(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := postTemplateJSON(t, srv.URL+"/templates",
		`{"name":"broken","rules":[{"type":"value_threshold","params":{"comparison":"nope","threshold":1}}]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", res.StatusCode, env)
	}
	if _, ok := detailReason(env, "rules[0].params.comparison"); !ok {
		t.Errorf("details = %+v, want the bad comparison to be named", env.Details)
	}
	if len(st.templates) != 0 {
		t.Errorf("a rejected template was stored: %+v", st.templates)
	}
}

// TestCreateTemplateAcceptsSubstitutedParams: a rule whose value comes from
// a parameter cannot be judged before the template is instantiated, so it is
// not rejected here.
func TestCreateTemplateAcceptsSubstitutedParams(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := postTemplateJSON(t, srv.URL+"/templates",
		`{"name":"templated","rules":[{"type":"value_threshold","params":{"comparison":"gt","threshold":"{{threshold}}"}}],`+
			`"parameters":[{"name":"threshold","required":true}]}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", res.StatusCode, env)
	}
	if len(st.templates) != 1 {
		t.Errorf("expected 1 template, got %d", len(st.templates))
	}
}

// TestUpdateTemplateRejectsUnknownRuleType: the same check on update.
func TestUpdateTemplateRejectsUnknownRuleType(t *testing.T) {
	st := &templateStore{templates: []store.MonitorTemplate{{ID: 1, Name: "t"}}, nextID: 1}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/templates/1",
		bytes.NewBufferString(`{"rules":[{"type":"not_a_rule","params":{}}]}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(st.templates[0].Rules) != 0 {
		t.Errorf("the rejected rules were stored: %+v", st.templates[0].Rules)
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
		known:  []store.Channel{{ID: 1, Name: "ops"}},
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
	// The monitor comes back with the rules and the channel links it was
	// created with, not as a bare monitor.
	if len(st.rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(st.rules))
	}
	if st.rules[0].Type != "token_event" {
		t.Errorf("rule type = %q, want token_event", st.rules[0].Type)
	}
	if got := st.channels[st.monitors[0].ID]; len(got) != 1 || got[0] != 1 {
		t.Errorf("channel links = %v, want [1]", got)
	}
}

// TestInstantiateTemplateMissingParam: a required parameter with no value is
// reported. It used to be a 400 with the same wording, so only the status
// changed: the request was understood, and what it carries cannot produce a
// valid monitor.
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
	res, env := postTemplateJSON(t, srv.URL+"/templates/1/instantiate", body)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %v", res.StatusCode, env)
	}
	if reason, ok := detailReason(env, "parameters.contract"); !ok || reason != "required parameter" {
		t.Errorf("details = %+v, want the missing parameter to be named", env.Details)
	}
	if len(st.monitors) != 0 {
		t.Errorf("a rejected instantiation created %d monitors", len(st.monitors))
	}
}

// TestInstantiateTemplateUnknownParameter: a parameter name the template
// does not declare is a typo, and it used to leave the placeholder
// unsubstituted so the rule it belonged to silently vanished.
func TestInstantiateTemplateUnknownParameter(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{
			{
				ID:         1,
				Name:       "token watcher",
				Rules:      []store.MonitorTemplateRule{{Type: "token_event", Params: json.RawMessage(`{"event":"{{contract}}"}`)}},
				Parameters: []store.TemplateParameter{{Name: "contract", Required: true}},
			},
		},
		nextID: 1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"my watcher","contract_ids":["CABCDEF"],"parameters":{"contractt":"CABCDEF"}}`
	res, env := postTemplateJSON(t, srv.URL+"/templates/1/instantiate", body)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %v", res.StatusCode, env)
	}
	if _, ok := detailReason(env, "parameters.contractt"); !ok {
		t.Errorf("details = %+v, want the misspelled parameter to be named", env.Details)
	}
	if len(st.monitors) != 0 {
		t.Errorf("a rejected instantiation created %d monitors", len(st.monitors))
	}
	if len(st.rules) != 0 {
		t.Errorf("a rejected instantiation created %d rules", len(st.rules))
	}
}

// TestInstantiateTemplateRejectsInvalidRule: a rule whose params do not
// validate once the parameters are substituted is reported by its index in
// the template, and nothing is written.
func TestInstantiateTemplateRejectsInvalidRule(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{
			{
				ID:   1,
				Name: "token watcher",
				Rules: []store.MonitorTemplateRule{
					{Type: "token_event", Params: json.RawMessage(`{"event":"transfer"}`)},
					{Type: "value_threshold", Params: json.RawMessage(`{"comparison":"gt","threshold":"{{threshold}}"}`)},
				},
				Parameters: []store.TemplateParameter{{Name: "threshold", Required: true}},
			},
		},
		nextID: 1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"my watcher","contract_ids":["CABCDEF"],"parameters":{"threshold":"not a number"}}`
	res, env := postTemplateJSON(t, srv.URL+"/templates/1/instantiate", body)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %v", res.StatusCode, env)
	}
	if _, ok := detailReason(env, "rules[1].params.threshold"); !ok {
		t.Errorf("details = %+v, want the second rule's threshold to be named", env.Details)
	}
	if len(st.monitors) != 0 {
		t.Errorf("a rejected instantiation created %d monitors", len(st.monitors))
	}
	if len(st.rules) != 0 {
		t.Errorf("a rejected instantiation created %d rules", len(st.rules))
	}
}

// TestInstantiateTemplateRejectsUnknownChannel: a channel that does not
// exist is reported instead of being dropped, which used to leave a monitor
// nobody hears from.
func TestInstantiateTemplateRejectsUnknownChannel(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{
			{
				ID:         1,
				Name:       "token watcher",
				ChannelIDs: []int64{7},
			},
		},
		nextID: 1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"my watcher","contract_ids":["CABCDEF"]}`
	res, env := postTemplateJSON(t, srv.URL+"/templates/1/instantiate", body)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %v", res.StatusCode, env)
	}
	if reason, ok := detailReason(env, "channel_ids[0]"); !ok || reason != "channel not found" {
		t.Errorf("details = %+v, want the missing channel to be named", env.Details)
	}
	if len(st.monitors) != 0 {
		t.Errorf("a rejected instantiation created %d monitors", len(st.monitors))
	}
}

// TestInstantiateTemplateStoreFailureLeavesNothing: a store failure while
// writing answers 500 and leaves no monitor behind.
func TestInstantiateTemplateStoreFailureLeavesNothing(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{{ID: 1, Name: "token watcher"}},
		nextID:    1,
		createErr: errStoreDown,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"name":"my watcher","contract_ids":["CABCDEF"]}`
	res, err := http.Post(srv.URL+"/templates/1/instantiate", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if len(st.monitors) != 0 {
		t.Errorf("a failed instantiation created %d monitors", len(st.monitors))
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
		known:  []store.Channel{{ID: 1, Name: "ops"}},
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
	if len(st.rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(st.rules))
	}
}

// TestBulkInstantiateTemplateValidatesEveryInstance: one bad instance fails
// the whole request, and nothing is written — the bulk handler validates
// everything before it writes anything.
// requestWorkspace returns the workspace a request's context is scoped to.
// A context that never went through AuthMiddleware carries none, which is
// the unconfigured single-tenant deployment: the default workspace.
func requestWorkspace(ctx context.Context) workspace.ID {
	if ws, ok := workspace.From(ctx); ok {
		return ws
	}
	return workspace.Default
}

// withTestWorkspace scopes each request to the workspace its
// X-Test-Workspace header names, the way AuthMiddleware scopes an
// authenticated request to its principal's workspace. An absent header
// resolves to the default workspace, as an unconfigured deployment does.
func withTestWorkspace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := workspace.Parse(r.Header.Get("X-Test-Workspace"))
		if err != nil {
			http.Error(w, "bad workspace", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r.WithContext(workspace.With(r.Context(), ws)))
	})
}

// getTemplateAs issues GET url, optionally scoped to one workspace.
func getTemplateAs(t *testing.T, url, ws string) (*http.Response, errorEnvelope) {
	t.Helper()
	res, _, env := getTemplateRaw(t, url, ws)
	return res, env
}

// getTemplateRaw is getTemplateAs with the undecoded body as well, so a test
// can search the exact bytes a client receives for text that must not leak.
func getTemplateRaw(t *testing.T, url, ws string) (*http.Response, []byte, errorEnvelope) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ws != "" {
		req.Header.Set("X-Test-Workspace", ws)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, raw)
	}
	return res, raw, env
}

// patchTemplateJSON sends a PATCH body to a template server and returns the
// response and its error envelope (empty on a 2xx body that is not an error).
func patchTemplateJSON(t *testing.T, url, body string) (*http.Response, errorEnvelope) {
	t.Helper()
	res, _, env := patchTemplateRaw(t, url, body)
	return res, env
}

// patchTemplateRaw is patchTemplateJSON with the undecoded body as well, so
// a test can search the exact bytes a client receives for leaked text.
func patchTemplateRaw(t *testing.T, url, body string) (*http.Response, []byte, errorEnvelope) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, raw)
	}
	return res, raw, env
}

// TestListTemplatesEmpty: an instance with no templates answers with an
// empty array, not null, so a client can index into it without a guard.
func TestListTemplatesEmpty(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, raw, _ := getTemplateRaw(t, srv.URL+"/templates", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Templates []store.MonitorTemplate `json:"templates"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, raw)
	}
	if body.Templates == nil {
		t.Error("templates is null; want an empty array")
	}
	if len(body.Templates) != 0 {
		t.Errorf("got %d templates, want 0", len(body.Templates))
	}
}

// TestListTemplatesSeveral: every stored template comes back with its shape
// intact, so a listing is enough to edit a template without fetching it.
func TestListTemplatesSeveral(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{
			{ID: 1, Name: "alpha", Description: "first", Rules: []store.MonitorTemplateRule{{Type: "event_emitted", Params: json.RawMessage(`{"event_name":"transfer"}`)}}, ChannelIDs: []int64{1}, Parameters: []store.TemplateParameter{{Name: "threshold", Required: true}}},
			{ID: 2, Name: "beta"},
			{ID: 3, Name: "gamma"},
		},
		nextID: 3,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, raw, _ := getTemplateRaw(t, srv.URL+"/templates", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Templates []store.MonitorTemplate `json:"templates"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, raw)
	}
	if len(body.Templates) != 3 {
		t.Fatalf("got %d templates, want 3", len(body.Templates))
	}
	if body.Templates[0].Name != "alpha" || body.Templates[0].Description != "first" {
		t.Errorf("first template = %+v, want alpha/first with its description", body.Templates[0])
	}
	if len(body.Templates[0].Rules) != 1 || body.Templates[0].Rules[0].Type != "event_emitted" {
		t.Errorf("first template's rules = %+v, want the event_emitted rule", body.Templates[0].Rules)
	}
	if len(body.Templates[0].ChannelIDs) != 1 || body.Templates[0].ChannelIDs[0] != 1 {
		t.Errorf("first template's channels = %+v, want [1]", body.Templates[0].ChannelIDs)
	}
	if len(body.Templates[0].Parameters) != 1 || body.Templates[0].Parameters[0].Name != "threshold" {
		t.Errorf("first template's parameters = %+v, want the threshold parameter", body.Templates[0].Parameters)
	}
}

// TestListTemplatesScopedToRequestWorkspace: the handler must hand the
// request's context to the store unchanged, because the store scopes its
// query to the workspace that context carries. A handler that dropped the
// context would hand one tenant another tenant's templates.
func TestListTemplatesScopedToRequestWorkspace(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(withTestWorkspace(newProbeServer(st, &fakeRPC{})))
	defer srv.Close()

	createIn := func(ws, name string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/templates",
			bytes.NewBufferString(`{"name":"`+name+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if ws != "" {
			req.Header.Set("X-Test-Workspace", ws)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(res.Body)
			t.Fatalf("create %q in %q: status = %d, body %s", name, ws, res.StatusCode, raw)
		}
	}

	createIn("acme", "acme watcher")
	createIn("beta", "beta watcher")
	createIn("", "default watcher")

	for _, tt := range []struct {
		hdr  string
		want string
	}{
		{"acme", "acme watcher"},
		{"beta", "beta watcher"},
		{"", "default watcher"},
	} {
		res, raw, env := getTemplateRaw(t, srv.URL+"/templates", tt.hdr)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("list as %q: status = %d: %v", tt.hdr, res.StatusCode, env)
		}
		var body struct {
			Templates []store.MonitorTemplate `json:"templates"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode: %v\nbody: %s", err, raw)
		}
		if len(body.Templates) != 1 {
			t.Fatalf("list as %q: got %d templates, want exactly its own one: %+v", tt.hdr, len(body.Templates), body.Templates)
		}
		if body.Templates[0].Name != tt.want {
			t.Errorf("list as %q: got %q, want %q", tt.hdr, body.Templates[0].Name, tt.want)
		}
	}
}

// TestListTemplatesStoreError: a store failure is a 500 whose body is the
// generic envelope — the driver's error text never reaches the client.
func TestListTemplatesStoreError(t *testing.T) {
	st := &templateStore{listErr: errStoreDown}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, raw, env := getTemplateRaw(t, srv.URL+"/templates", "")
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if env.Error != "internal error" {
		t.Errorf("error = %q, want the generic internal error", env.Error)
	}
	if strings.Contains(string(raw), errStoreDown.Error()) {
		t.Errorf("the response leaked the store's error text: %s", raw)
	}
}

// TestGetTemplateFound: the full template, shape intact.
func TestGetTemplateFound(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{{
			ID: 7, Name: "token watcher", Description: "watches transfers",
			Rules:      []store.MonitorTemplateRule{{Type: "token_event", Params: json.RawMessage(`{"event":"transfer"}`)}},
			ChannelIDs: []int64{1, 2},
			Parameters: []store.TemplateParameter{{Name: "threshold", Default: "1000"}},
		}},
		nextID: 7,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, raw, env := getTemplateRaw(t, srv.URL+"/templates/7", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", res.StatusCode, env)
	}
	var got store.MonitorTemplate
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, raw)
	}
	if got.ID != 7 || got.Name != "token watcher" || got.Description != "watches transfers" {
		t.Errorf("template = %+v, want id 7 and its name and description", got)
	}
	if len(got.Rules) != 1 || got.Rules[0].Type != "token_event" {
		t.Errorf("rules = %+v, want the token_event rule", got.Rules)
	}
	if len(got.ChannelIDs) != 2 || got.ChannelIDs[1] != 2 {
		t.Errorf("channel_ids = %+v, want [1 2]", got.ChannelIDs)
	}
	if len(got.Parameters) != 1 || got.Parameters[0].Default != "1000" {
		t.Errorf("parameters = %+v, want the threshold default", got.Parameters)
	}
}

// TestGetTemplateNotFound: an id the workspace cannot see answers exactly as
// an id that was never issued.
func TestGetTemplateNotFound(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := getTemplateAs(t, srv.URL+"/templates/99", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", res.StatusCode, env)
	}
	if env.Error != "template not found" {
		t.Errorf("error = %q, want template not found", env.Error)
	}
}

// TestGetTemplateMalformedID: an id that is not a number is a 400, not a
// lookup that fails and not a 500 from the parse.
func TestGetTemplateMalformedID(t *testing.T) {
	st := &templateStore{templates: []store.MonitorTemplate{{ID: 1, Name: "t"}}, nextID: 1}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	for _, id := range []string{"abc", "1.5", "1e3"} {
		res, env := getTemplateAs(t, srv.URL+"/templates/"+id, "")
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /templates/%s: status = %d, want 400: %v", id, res.StatusCode, env)
		}
	}
}

// TestGetTemplateStoreError: a store failure on the read path is a 500 with
// the generic envelope.
func TestGetTemplateStoreError(t *testing.T) {
	st := &templateStore{getErr: errStoreDown}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, raw, env := getTemplateRaw(t, srv.URL+"/templates/1", "")
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if env.Error != "internal error" {
		t.Errorf("error = %q, want the generic internal error", env.Error)
	}
	if strings.Contains(string(raw), errStoreDown.Error()) {
		t.Errorf("the response leaked the store's error text: %s", raw)
	}
}

// TestUpdateTemplatePartial: PATCH merges field by field — only what the
// request carries changes.
func TestUpdateTemplatePartial(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{{
			ID: 1, Name: "old name", Description: "old description",
			Rules:      []store.MonitorTemplateRule{{Type: "token_event", Params: json.RawMessage(`{"event":"transfer"}`)}},
			ChannelIDs: []int64{1, 2},
			Parameters: []store.TemplateParameter{{Name: "threshold", Required: true}},
		}},
		nextID: 1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := patchTemplateJSON(t, srv.URL+"/templates/1", `{"name":"new name"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", res.StatusCode, env)
	}
	got := st.templates[0]
	if got.Name != "new name" {
		t.Errorf("name = %q, want new name", got.Name)
	}
	if got.Description != "old description" {
		t.Errorf("description = %q; a request that did not carry it must not change it", got.Description)
	}
	if len(got.Rules) != 1 || got.Rules[0].Type != "token_event" {
		t.Errorf("rules = %+v; a request that did not carry them must not change them", got.Rules)
	}
	if len(got.ChannelIDs) != 2 {
		t.Errorf("channel_ids = %+v; a request that did not carry them must not change them", got.ChannelIDs)
	}
	if len(got.Parameters) != 1 || got.Parameters[0].Name != "threshold" {
		t.Errorf("parameters = %+v; a request that did not carry them must not change them", got.Parameters)
	}
}

// TestUpdateTemplateEmptyValuesLeaveFieldsAlone pins the merge semantics the
// issue asks about: an empty string and a null slice mean "leave unchanged",
// so today there is no way to clear a description or empty the channel list
// with null — only with an explicit empty array for the slices.
func TestUpdateTemplateEmptyValuesLeaveFieldsAlone(t *testing.T) {
	seed := func() *templateStore {
		return &templateStore{
			templates: []store.MonitorTemplate{{
				ID: 1, Name: "old name", Description: "old description",
				Rules:      []store.MonitorTemplateRule{{Type: "token_event", Params: json.RawMessage(`{"event":"transfer"}`)}},
				ChannelIDs: []int64{1, 2},
				Parameters: []store.TemplateParameter{{Name: "threshold", Required: true}},
			}},
			nextID: 1,
		}
	}

	for _, tt := range []struct {
		name  string
		body  string
		check func(*testing.T, store.MonitorTemplate)
	}{
		{
			name: "empty description leaves the description alone",
			body: `{"description":""}`,
			check: func(t *testing.T, got store.MonitorTemplate) {
				if got.Description != "old description" {
					t.Errorf("description = %q, want it left as old description", got.Description)
				}
			},
		},
		{
			name: "null rules leave the rules alone",
			body: `{"rules":null}`,
			check: func(t *testing.T, got store.MonitorTemplate) {
				if len(got.Rules) != 1 {
					t.Errorf("rules = %+v, want the original rule kept", got.Rules)
				}
			},
		},
		{
			name: "null channel_ids leave the channels alone",
			body: `{"channel_ids":null}`,
			check: func(t *testing.T, got store.MonitorTemplate) {
				if len(got.ChannelIDs) != 2 {
					t.Errorf("channel_ids = %+v, want [1 2] kept", got.ChannelIDs)
				}
			},
		},
		{
			name: "empty rules replace the rules",
			body: `{"rules":[]}`,
			check: func(t *testing.T, got store.MonitorTemplate) {
				if len(got.Rules) != 0 {
					t.Errorf("rules = %+v, want them cleared by an explicit empty array", got.Rules)
				}
			},
		},
		{
			name: "empty channel_ids replace the channels",
			body: `{"channel_ids":[]}`,
			check: func(t *testing.T, got store.MonitorTemplate) {
				if len(got.ChannelIDs) != 0 {
					t.Errorf("channel_ids = %+v, want them cleared by an explicit empty array", got.ChannelIDs)
				}
			},
		},
		{
			name: "empty parameters replace the parameters",
			body: `{"parameters":[]}`,
			check: func(t *testing.T, got store.MonitorTemplate) {
				if len(got.Parameters) != 0 {
					t.Errorf("parameters = %+v, want them cleared by an explicit empty array", got.Parameters)
				}
			},
		},
		{
			name: "new rules replace the rules",
			body: `{"rules":[{"type":"event_emitted","params":{"event_name":"burn"}}]}`,
			check: func(t *testing.T, got store.MonitorTemplate) {
				if len(got.Rules) != 1 || got.Rules[0].Type != "event_emitted" {
					t.Errorf("rules = %+v, want the replacement rule", got.Rules)
				}
				if got.Name != "old name" {
					t.Errorf("name = %q; a request that did not carry it must not change it", got.Name)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := seed()
			srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
			defer srv.Close()

			res, env := patchTemplateJSON(t, srv.URL+"/templates/1", tt.body)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %v", res.StatusCode, env)
			}
			tt.check(t, st.templates[0])
		})
	}
}

// TestUpdateTemplateNotFound: updating an id that does not exist is a 404
// and writes nothing.
func TestUpdateTemplateNotFound(t *testing.T) {
	st := &templateStore{}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := patchTemplateJSON(t, srv.URL+"/templates/42", `{"name":"x"}`)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", res.StatusCode, env)
	}
	if len(st.templates) != 0 {
		t.Errorf("a rejected update created templates: %+v", st.templates)
	}
}

// TestUpdateTemplateMalformedID: an id that is not a number is a 400.
func TestUpdateTemplateMalformedID(t *testing.T) {
	st := &templateStore{templates: []store.MonitorTemplate{{ID: 1, Name: "t"}}, nextID: 1}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := patchTemplateJSON(t, srv.URL+"/templates/not-a-number", `{"name":"x"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", res.StatusCode, env)
	}
	if st.templates[0].Name != "t" {
		t.Errorf("name = %q; a rejected update must change nothing", st.templates[0].Name)
	}
}

// TestUpdateTemplateStoreError: a store failure on the write path is a 500
// with the generic envelope, and the failed update leaves the stored
// template as it was.
func TestUpdateTemplateStoreError(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{{ID: 1, Name: "old name"}},
		nextID:    1,
		updateErr: errStoreDown,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := patchTemplateJSON(t, srv.URL+"/templates/1", `{"name":"new name"}`)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %v", res.StatusCode, env)
	}
	if env.Error != "internal error" {
		t.Errorf("error = %q, want the generic internal error", env.Error)
	}
	if st.templates[0].Name != "old name" {
		t.Errorf("name = %q; a failed write must leave the template as it was", st.templates[0].Name)
	}
}

// TestUpdateTemplateRevalidatesMergedRules: the merged result, not just the
// incoming rules, is validated, so a patch that keeps a broken rule cannot
// land it either.
func TestUpdateTemplateRevalidatesMergedRules(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{{ID: 1, Name: "t"}},
		nextID:    1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, env := patchTemplateJSON(t, srv.URL+"/templates/1",
		`{"rules":[{"type":"value_threshold","params":{"comparison":"nope","threshold":1}}]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", res.StatusCode, env)
	}
	if _, ok := detailReason(env, "rules[0].params.comparison"); !ok {
		t.Errorf("details = %+v, want the bad comparison named", env.Details)
	}
	if len(st.templates[0].Rules) != 0 {
		t.Errorf("the rejected rules were stored: %+v", st.templates[0].Rules)
	}
}

// TestUpdateTemplateMalformedJSON: a body that is not JSON is a 400 and
// changes nothing.
func TestUpdateTemplateMalformedJSON(t *testing.T) {
	st := &templateStore{
		templates: []store.MonitorTemplate{{ID: 1, Name: "old name"}},
		nextID:    1,
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, _ := patchTemplateJSON(t, srv.URL+"/templates/1", `{"name":`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if st.templates[0].Name != "old name" {
		t.Errorf("name = %q; a malformed body must change nothing", st.templates[0].Name)
	}
}

// TestUpdateTemplateFetchStoreError: the read that precedes the write fails
// the same way the write does — a 500 with the generic envelope.
func TestUpdateTemplateFetchStoreError(t *testing.T) {
	st := &templateStore{getErr: errStoreDown}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	res, raw, env := patchTemplateRaw(t, srv.URL+"/templates/1", `{"name":"x"}`)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if env.Error != "internal error" {
		t.Errorf("error = %q, want the generic internal error", env.Error)
	}
	if strings.Contains(string(raw), errStoreDown.Error()) {
		t.Errorf("the response leaked the store's error text: %s", raw)
	}
}

func TestBulkInstantiateTemplateValidatesEveryInstance(t *testing.T) {
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
		known:  []store.Channel{{ID: 1, Name: "ops"}},
	}
	srv := httptest.NewServer(newProbeServer(st, &fakeRPC{}))
	defer srv.Close()

	body := `{"instances":[{"name":"w1","contract_ids":["C1"]},{"name":"w2"}]}`
	res, env := postTemplateJSON(t, srv.URL+"/templates/1/instantiate/bulk", body)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %v", res.StatusCode, env)
	}
	if _, ok := detailReason(env, "instances[1].contract_ids"); !ok {
		t.Errorf("details = %+v, want the second instance's problem to be named", env.Details)
	}
	if len(st.monitors) != 0 {
		t.Errorf("a rejected bulk instantiation created %d monitors", len(st.monitors))
	}
}
