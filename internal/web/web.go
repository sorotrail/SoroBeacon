// Package web serves the server-rendered htmx dashboard.
//
// It is intentionally read-and-basic-write; a richer SPA is left as a
// contributor issue. Handlers call the store directly and re-render whole
// pages (plain form POST + redirect), with htmx used for inline actions
// like channel test-sends.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sorobeacon/sorobeacon/internal/notify"
	"github.com/sorobeacon/sorobeacon/internal/rules"
	"github.com/sorobeacon/sorobeacon/internal/stellar"
	"github.com/sorobeacon/sorobeacon/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Server renders the dashboard.
type Server struct {
	store    store.Store
	registry *rules.Registry
	factory  *notify.Factory
	log      *slog.Logger
	pages    map[string]*template.Template
}

// New parses templates and wires a dashboard server.
func New(st store.Store, reg *rules.Registry, f *notify.Factory, log *slog.Logger) (*Server, error) {
	s := &Server{store: st, registry: reg, factory: f, log: log, pages: map[string]*template.Template{}}
	for _, page := range []string{"index", "monitors", "monitor", "channels", "alerts"} {
		t, err := template.ParseFS(templatesFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

// Routes returns the dashboard router, mounted at / by cmd/sorobeacon.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", s.index)

	r.Get("/monitors", s.monitors)
	r.Post("/monitors", s.createMonitor)
	r.Get("/monitors/{id}", s.monitorDetail)
	r.Post("/monitors/{id}/toggle", s.toggleMonitor)
	r.Post("/monitors/{id}/delete", s.deleteMonitor)
	r.Post("/monitors/{id}/rules", s.createRule)
	r.Post("/monitors/{id}/rules/{ruleID}/delete", s.deleteRule)
	r.Post("/monitors/{id}/channels", s.setMonitorChannels)

	r.Get("/channels", s.channels)
	r.Post("/channels", s.createChannel)
	r.Post("/channels/{id}/delete", s.deleteChannel)
	r.Post("/channels/{id}/test", s.testChannel)

	r.Get("/alerts", s.alerts)
	return r
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render page", "page", page, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("web error", "err", err)
	http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
}

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, name), 10, 64)
}

// --- pages ---

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.GetStats(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	alerts, err := s.store.ListAlerts(r.Context(), store.AlertFilter{Limit: 10})
	if err != nil {
		s.fail(w, err)
		return
	}
	names, err := s.monitorNames(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "index", map[string]any{
		"Title": "Overview", "Stats": stats, "Alerts": alerts, "MonitorNames": names,
	})
}

func (s *Server) monitors(w http.ResponseWriter, r *http.Request) {
	monitors, err := s.store.ListMonitors(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "monitors", map[string]any{"Title": "Monitors", "Monitors": monitors})
}

func (s *Server) createMonitor(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	contracts := splitLines(r.FormValue("contract_ids"))
	if name == "" || len(contracts) == 0 {
		http.Error(w, "name and contract_ids are required", http.StatusBadRequest)
		return
	}
	for _, c := range contracts {
		if !stellar.IsValidContractID(c) {
			http.Error(w, "invalid contract id: "+c, http.StatusBadRequest)
			return
		}
	}
	m := store.Monitor{Name: name, ContractIDs: contracts, Enabled: true}
	if err := s.store.CreateMonitor(r.Context(), &m); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/monitors", http.StatusSeeOther)
}

func (s *Server) monitorDetail(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ruleList, err := s.store.ListRules(r.Context(), id, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	channels, err := s.store.ListChannels(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	attached := map[int64]bool{}
	for _, cid := range m.ChannelIDs {
		attached[cid] = true
	}
	s.render(w, "monitor", map[string]any{
		"Title": m.Name, "Monitor": m, "Rules": ruleList,
		"Channels": channels, "Attached": attached, "RuleTypes": s.registry.Types(),
	})
}

func (s *Server) toggleMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m.Enabled = !m.Enabled
	if err := s.store.UpdateMonitor(r.Context(), m); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/monitors", http.StatusSeeOther)
}

func (s *Server) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteMonitor(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/monitors", http.StatusSeeOther)
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ruleType := r.FormValue("type")
	params := []byte(r.FormValue("params"))
	if len(params) == 0 {
		params = []byte(`{}`)
	}
	if err := s.registry.Validate(ruleType, params); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rule := store.Rule{MonitorID: id, Type: ruleType, Params: params, Enabled: true}
	if err := s.store.CreateRule(r.Context(), &rule); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/monitors/%d", id), http.StatusSeeOther)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ruleID, err := pathID(r, "ruleID")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rule, err := s.store.GetRule(r.Context(), ruleID)
	if err != nil || rule.MonitorID != id {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteRule(r.Context(), ruleID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/monitors/%d", id), http.StatusSeeOther)
}

func (s *Server) setMonitorChannels(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var channelIDs []int64
	for _, v := range r.PostForm["channel_ids"] {
		cid, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "invalid channel id", http.StatusBadRequest)
			return
		}
		channelIDs = append(channelIDs, cid)
	}
	if err := s.store.SetMonitorChannels(r.Context(), id, channelIDs); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/monitors/%d", id), http.StatusSeeOther)
}

func (s *Server) channels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.store.ListChannels(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "channels", map[string]any{
		"Title": "Channels", "Channels": channels, "ChannelTypes": s.factory.Types(),
	})
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	chType := r.FormValue("type")
	config := []byte(r.FormValue("config"))
	if len(config) == 0 {
		config = []byte(`{}`)
	}
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if _, err := s.factory.New(chType, config); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ch := store.Channel{Name: name, Type: chType, Config: config, Enabled: true}
	if err := s.store.CreateChannel(r.Context(), &ch); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/channels", http.StatusSeeOther)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteChannel(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/channels", http.StatusSeeOther)
}

// testChannel is the htmx target for the "Send test" button; it returns a
// small HTML fragment rather than a page.
func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	notifier, err := s.factory.New(ch.Type, ch.Config)
	if err == nil {
		err = notifier.Send(r.Context(), notify.Alert{
			MonitorName: "Test monitor",
			RuleType:    "test",
			EventName:   "sorobeacon_test",
			EventID:     "test-0000000000000000000",
			CreatedAt:   time.Now(),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		fmt.Fprintf(w, "❌ %s", template.HTMLEscapeString(err.Error()))
		return
	}
	fmt.Fprint(w, "✅ sent")
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AlertFilter{Limit: 50}
	var selected int64
	if v := q.Get("monitor_id"); v != "" {
		selected, _ = strconv.ParseInt(v, 10, 64)
		f.MonitorID = selected
	}
	if v := q.Get("cursor"); v != "" {
		f.AfterID, _ = strconv.ParseInt(v, 10, 64)
	}
	alerts, err := s.store.ListAlerts(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	monitors, err := s.store.ListMonitors(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	names := map[int64]string{}
	for _, m := range monitors {
		names[m.ID] = m.Name
	}
	next := ""
	if len(alerts) == f.Limit {
		next = strconv.FormatInt(alerts[len(alerts)-1].ID, 10)
	}
	s.render(w, "alerts", map[string]any{
		"Title": "Alerts", "Alerts": alerts, "Monitors": monitors,
		"MonitorNames": names, "SelectedMonitor": selected, "NextCursor": next,
	})
}

func (s *Server) monitorNames(r *http.Request) (map[int64]string, error) {
	monitors, err := s.store.ListMonitors(r.Context(), false)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	for _, m := range monitors {
		names[m.ID] = m.Name
	}
	return names, nil
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' '
	}) {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
