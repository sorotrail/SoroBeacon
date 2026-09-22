// Package web serves the server-rendered htmx dashboard.
//
// It is intentionally read-and-basic-write; a richer SPA is left as a
// contributor issue. Handlers call the store directly and re-render whole
// pages (plain form POST + redirect), with htmx used for inline actions
// like channel test-sends.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/buildinfo"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// PositionReader is the poller's race-free snapshot of ingest progress.
type PositionReader interface {
	Position() poller.Position
}

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed favicon.svg
var faviconSVG []byte

// Server renders the dashboard.
type Server struct {
	store    store.Store
	registry *rules.Registry
	factory  *notify.Factory
	log      *slog.Logger
	pages    map[string]*template.Template
	poller   PositionReader
}

// templateFuncs are available to every page template.
var templateFuncs = template.FuncMap{
	"prettyJSON":   prettyJSON,
	"formatTime":   formatTime,
	"decodedEvent": decodedEvent,
	"truncateID":   truncateID,
}

const tsLayout = "2006-01-02 15:04:05"

// formatTime is the only dashboard timestamp renderer. The server always
// emits labelled UTC so a viewer with JavaScript disabled still knows the
// zone; when the preference is "local", a data-tz attribute lets the
// browser rewrite the visible text to the viewer's zone after load.
func formatTime(t time.Time, tz string) template.HTML {
	if t.IsZero() {
		return ""
	}
	utc := t.UTC()
	label := utc.Format(tsLayout) + " UTC"
	return template.HTML(fmt.Sprintf(
		`<time datetime="%s" data-tz="%s">%s</time>`,
		template.HTMLEscapeString(utc.Format(time.RFC3339)),
		template.HTMLEscapeString(parseTZ(tz)),
		template.HTMLEscapeString(label),
	))
}

// prettyJSON indents raw JSON for display. Invalid or empty input falls
// back to the raw string rather than erroring the page — a payload is
// still worth showing even if it turns out not to parse.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if len(raw) == 0 {
		return string(raw)
	}
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// truncateID keeps `keep` runes from each end of s, joined with an
// ellipsis, so both identifying ends of a long contract or event ID stay
// visible in a table cell. Strings that already fit in 2*keep runes (the
// threshold at which truncation would not shorten the value) are returned
// unchanged, including the empty string.
func truncateID(s string, keep int) string {
	if keep <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= 2*keep {
		return s
	}
	return string(r[:keep]) + "…" + string(r[len(r)-keep:])
}

// New parses templates and wires a dashboard server.
func New(st store.Store, reg *rules.Registry, f *notify.Factory, log *slog.Logger) (*Server, error) {
	s := &Server{store: st, registry: reg, factory: f, log: log, pages: map[string]*template.Template{}}
	for _, page := range []string{"index", "monitors", "monitor", "channels", "alerts", "alert", "error"} {
		t, err := template.New("layout.html").Funcs(templateFuncs).ParseFS(templatesFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

// WithPoller attaches the ingest-position source shown on the overview page.
func (s *Server) WithPoller(p PositionReader) *Server {
	s.poller = p
	return s
}

// Routes returns the dashboard router, mounted at / by cmd/sorobeacon.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", s.index)
	r.Get("/favicon.ico", s.favicon)
	r.Post("/theme", s.setTheme)
	r.Post("/timezone", s.setTimezone)

	r.Get("/monitors", s.monitors)
	r.Post("/monitors", s.createMonitor)
	r.Post("/monitors/bulk", s.bulkMonitors)
	r.Get("/monitors/{id}", s.monitorDetail)
	r.Post("/monitors/{id}/toggle", s.toggleMonitor)
	r.Post("/monitors/{id}/delete", s.deleteMonitor)
	r.Post("/monitors/{id}/duplicate", s.duplicateMonitor)
	r.Post("/monitors/{id}/rules", s.createRule)
	r.Post("/monitors/{id}/rules/{ruleID}/toggle", s.toggleRule)
	r.Post("/monitors/{id}/rules/{ruleID}/delete", s.deleteRule)
	r.Post("/monitors/{id}/channels", s.setMonitorChannels)

	r.Get("/channels", s.channels)
	r.Post("/channels", s.createChannel)
	r.Post("/channels/{id}/delete", s.deleteChannel)
	r.Post("/channels/{id}/test", s.testChannel)

	r.Get("/alerts", s.alerts)
	r.Get("/alerts/{id}/deliveries", s.alertDeliveries)
	r.Post("/alerts/{id}/deliveries/{channelID}/retry", s.retryDelivery)
	r.Get("/alerts/{id}", s.alertDetail)
	r.NotFound(s.notFound)
	return r
}

// navSection maps a page name to the header nav entry it highlights.
// "monitor" (the detail page) highlights the same entry as "monitors" —
// they're the same section as far as navigation is concerned. "index"
// intentionally maps to "" (Overview has no distinct nav highlight of its
// own beyond the brand link).
var navSection = map[string]string{
	"monitors": "monitors",
	"monitor":  "monitors",
	"channels": "channels",
	"alerts":   "alerts",
	"alert":    "alerts",
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	s.renderStatus(w, r, http.StatusOK, page, data)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	if m, ok := data.(map[string]any); ok {
		if _, exists := m["Active"]; !exists {
			m["Active"] = navSection[page]
		}
		m["Theme"] = themeFromRequest(r)
		m["Timezone"] = tzFromRequest(r)
		m["Version"] = displayVersion(buildinfo.Version)
		m["Commit"] = displayCommit(buildinfo.Commit)
		m["CommitURL"] = commitURL(buildinfo.Commit)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := s.pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render page", "page", page, "err", err)
	}
}

const (
	themeCookie       = "theme"
	themeCookieMaxAge = 365 * 24 * 3600
)

// parseTheme allowlists the three dashboard theme states. Anything else
// (missing cookie, typos, empty) is "system" so existing users keep the
// OS-follow default.
func parseTheme(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "light":
		return "light"
	case "dark":
		return "dark"
	default:
		return "system"
	}
}

func themeFromRequest(r *http.Request) string {
	if r == nil {
		return "system"
	}
	c, err := r.Cookie(themeCookie)
	if err != nil {
		return "system"
	}
	return parseTheme(c.Value)
}

const (
	tzCookie       = "tz"
	tzCookieMaxAge = 365 * 24 * 3600
)

// parseTZ allowlists the two dashboard timezone states. Anything else
// (missing cookie, typos, empty) is UTC so existing unlabelled times
// become labelled UTC rather than silently switching zone.
func parseTZ(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "local":
		return "local"
	default:
		return "utc"
	}
}

func tzFromRequest(r *http.Request) string {
	if r == nil {
		return "utc"
	}
	c, err := r.Cookie(tzCookie)
	if err != nil {
		return "utc"
	}
	return parseTZ(c.Value)
}

// safeReturn keeps the timezone POST from bouncing to an external Referer.
func safeReturn(r *http.Request) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/"
	}
	if u.Host != "" && u.Host != r.Host {
		return "/"
	}
	p := u.RequestURI()
	if p == "" || !strings.HasPrefix(p, "/") {
		return "/"
	}
	return p
}

// setTheme persists the dashboard theme in a cookie and redirects back so
// the next render already has the right data-theme (no flash of the other
// scheme from a client-side fix-up).
func (s *Server) setTheme(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	theme := parseTheme(r.FormValue("theme"))
	http.SetCookie(w, &http.Cookie{
		Name:     themeCookie,
		Value:    theme,
		Path:     "/",
		MaxAge:   themeCookieMaxAge,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeReturn(r), http.StatusSeeOther)
}

// setTimezone persists the dashboard timezone in a cookie and redirects
// back so the next render already has the right data-tz values.
func (s *Server) setTimezone(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tz := parseTZ(r.FormValue("tz"))
	http.SetCookie(w, &http.Cookie{
		Name:     tzCookie,
		Value:    tz,
		Path:     "/",
		MaxAge:   tzCookieMaxAge,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeReturn(r), http.StatusSeeOther)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("web error", "err", err)
	s.renderStatus(w, nil, http.StatusInternalServerError, "error", map[string]any{
		"Title":   "Error",
		"Heading": "Something went wrong",
		"Message": "internal error: " + err.Error(),
	})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.renderStatus(w, r, http.StatusNotFound, "error", map[string]any{
		"Title":   "Not found",
		"Heading": "Not found",
		"Message": "That page does not exist.",
	})
}

func displayVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "dev"
	}
	return v
}

func displayCommit(c string) string {
	if strings.TrimSpace(c) == "" {
		return "none"
	}
	return c
}

// commitURL is the GitHub commit page when Commit looks like a real SHA,
// otherwise empty so the footer renders the value as plain text.
func commitURL(commit string) string {
	if !isGitSHA(commit) {
		return ""
	}
	return "https://github.com/sorotrail/SoroBeacon/commit/" + commit
}

func isGitSHA(s string) bool {
	if n := len(s); n < 7 || n > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, name), 10, 64)
}

// favicon serves the embedded icon so browsers requesting /favicon.ico on
// every page load stop logging 404s. Served with a long cache lifetime:
// the icon is baked into the binary, so a new version always ships with a
// new deploy anyway.
func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	if _, err := w.Write(faviconSVG); err != nil {
		s.log.Warn("writing favicon response", "err", err)
	}
}

// --- pages ---

// emptyKind classifies a page's empty state. Distinct cases so a fresh
// install is not shown the same "nothing here" copy as a healthy instance
// that simply has not matched an event yet.
func emptyKind(monitors []store.Monitor, channels []store.Channel, alerts []store.Alert) string {
	if len(alerts) > 0 {
		return ""
	}
	if len(monitors) == 0 {
		return "no_monitors"
	}
	enabled := false
	for _, m := range monitors {
		if m.Enabled {
			enabled = true
			break
		}
	}
	if !enabled {
		return "monitors_disabled"
	}
	if len(channels) == 0 {
		return "no_channels"
	}
	return "no_alerts"
}

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
	monitors, err := s.store.ListMonitors(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	channels, err := s.store.ListChannels(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	names := map[int64]string{}
	for _, m := range monitors {
		names[m.ID] = m.Name
	}
	data := map[string]any{
		"Title": "Overview", "Stats": stats, "Alerts": alerts, "MonitorNames": names,
		"Empty": emptyKind(monitors, channels, alerts),
	}
	if s.poller != nil {
		if pos := s.poller.Position(); pos.Ready() {
			data["Poller"] = pos
		}
	}
	s.render(w, r, "index", data)
}

func (s *Server) monitors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListFilter{Limit: 50, Query: strings.TrimSpace(q.Get("q"))}
	switch q.Get("enabled") {
	case "true":
		t := true
		f.Enabled = &t
		f.EnabledOnly = true
	case "false":
		t := false
		f.Enabled = &t
	}
	switch q.Get("sort") {
	case "id", "created_at", "name":
		f.Sort = q.Get("sort")
	}
	if v := q.Get("cursor"); v != "" {
		f.AfterID, _ = strconv.ParseInt(v, 10, 64)
	}
	monitors, err := s.store.ListMonitorsPage(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	empty := ""
	if len(monitors) == 0 {
		empty = "no_monitors"
	} else {
		anyEnabled := false
		for _, m := range monitors {
			if m.Enabled {
				anyEnabled = true
				break
			}
		}
		if !anyEnabled {
			empty = "monitors_disabled"
		}
	}
	next := ""
	if len(monitors) == f.Limit {
		next = strconv.FormatInt(monitors[len(monitors)-1].ID, 10)
	}
	enabled := q.Get("enabled")
	sort := f.Sort
	if sort == "" {
		sort = "name"
	}
	data := map[string]any{
		"Title": "Monitors", "Monitors": monitors, "NextCursor": next,
		"Empty": empty,
		"Query": f.Query, "Enabled": enabled, "Sort": sort,
	}
	if next != "" {
		// template.URL so q/enabled/sort query separators are not %26-escaped.
		data["OlderHref"] = template.URL("/monitors?" + monitorFilterQuery(f.Query, enabled, f.Sort) + "cursor=" + next)
	}
	s.render(w, r, "monitors", data)
}

// monitorFilterQuery is the q/enabled/sort prefix preserved on the Older
// paging link so filters survive navigation. Empty when every control is
// at its default, so the existing `?cursor=` link stays stable.
func monitorFilterQuery(q, enabled, sort string) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if enabled == "true" || enabled == "false" {
		v.Set("enabled", enabled)
	}
	if sort != "" && sort != "name" {
		v.Set("sort", sort)
	}
	enc := v.Encode()
	if enc == "" {
		return ""
	}
	return enc + "&"
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
		s.notFound(w, r)
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if err != nil {
		s.notFound(w, r)
		return
	}
	ruleList, err := s.store.ListRules(r.Context(), id, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	channels, err := s.store.ListChannels(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	attached := map[int64]bool{}
	for _, cid := range m.ChannelIDs {
		attached[cid] = true
	}
	s.render(w, r, "monitor", map[string]any{
		"Title": m.Name, "Monitor": m, "Rules": ruleList,
		"Channels": channels, "Attached": attached, "RuleTypes": s.registry.Types(),
	})
}

func (s *Server) bulkMonitors(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	enabledStr := r.FormValue("enabled")
	if enabledStr != "true" && enabledStr != "false" {
		http.Error(w, "enabled is required", http.StatusBadRequest)
		return
	}
	raw := r.Form["ids"]
	if len(raw) == 0 {
		http.Error(w, "ids is required", http.StatusBadRequest)
		return
	}
	ids := make([]int64, 0, len(raw))
	for _, v := range raw {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
	}
	if _, _, err := s.store.SetMonitorsEnabled(r.Context(), ids, enabledStr == "true"); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/monitors", http.StatusSeeOther)
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

func (s *Server) duplicateMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := s.store.DuplicateMonitor(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/monitors/%d", m.ID), http.StatusSeeOther)
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

func (s *Server) toggleRule(w http.ResponseWriter, r *http.Request) {
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
	rule.Enabled = !rule.Enabled
	if err := s.store.UpdateRule(r.Context(), rule); err != nil {
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
	f := store.ListFilter{Limit: 50}
	if v := r.URL.Query().Get("cursor"); v != "" {
		f.AfterID, _ = strconv.ParseInt(v, 10, 64)
	}
	channels, err := s.store.ListChannelsPage(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	monitors, err := s.store.ListMonitors(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	empty := ""
	if len(channels) == 0 {
		empty = "no_channels"
	}
	next := ""
	if len(channels) == f.Limit {
		next = strconv.FormatInt(channels[len(channels)-1].ID, 10)
	}
	s.render(w, r, "channels", map[string]any{
		"Title": "Channels", "Channels": channels, "ChannelTypes": s.factory.Types(),
		"Empty": empty, "HasMonitors": len(monitors) > 0,
		"NextCursor": next,
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
	var selectedRule int64
	if v := q.Get("rule_id"); v != "" {
		selectedRule, _ = strconv.ParseInt(v, 10, 64)
		f.RuleID = selectedRule
	}
	f.ContractID = strings.TrimSpace(q.Get("contract_id"))
	switch q.Get("sort") {
	case "created_at_asc", "created_at_desc":
		f.Sort = q.Get("sort")
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
	sort := f.Sort
	if sort == "" {
		sort = "created_at_desc"
	}
	// Channels are only needed to tell the empty states apart: "no alerts
	// yet" reads very differently when nothing is being watched, when every
	// monitor is off, and when there is nowhere to deliver to.
	channels, err := s.store.ListChannels(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	data := map[string]any{
		"Title": "Alerts", "Alerts": alerts, "Monitors": monitors,
		"MonitorNames": names, "SelectedMonitor": selected,
		"SelectedRule": selectedRule, "ContractID": f.ContractID, "Sort": sort,
		"Empty": emptyKind(monitors, channels, alerts),
	}
	if next != "" {
		// template.URL so filter query separators are not %26-escaped.
		data["OlderHref"] = template.URL("/alerts?" + alertFilterQuery(selected, selectedRule, f.ContractID, f.Sort) + "cursor=" + next)
	}
	s.render(w, r, "alerts", data)
}

// alertFilterQuery is the monitor/rule/contract/sort prefix preserved on
// the Older paging link. Empty when every control is at its default, so
// the existing `?cursor=` link stays stable.
func alertFilterQuery(monitorID, ruleID int64, contractID, sort string) string {
	v := url.Values{}
	if monitorID != 0 {
		v.Set("monitor_id", strconv.FormatInt(monitorID, 10))
	}
	if ruleID != 0 {
		v.Set("rule_id", strconv.FormatInt(ruleID, 10))
	}
	if contractID != "" {
		v.Set("contract_id", contractID)
	}
	if sort != "" && sort != "created_at_desc" {
		v.Set("sort", sort)
	}
	enc := v.Encode()
	if enc == "" {
		return ""
	}
	return enc + "&"
}

// alertDeliveries serves an htmx fragment of an alert's delivery history
// (channel, status, response snippet, timestamp), loaded lazily when a row
// on /alerts is expanded rather than eagerly for every alert on the page.
func (s *Server) alertDeliveries(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.writeDeliveriesFragment(w, r, id, "")
}

// retryDelivery re-sends one failed attempt and swaps the deliveries
// fragment in place. Gate failures stay in the fragment so htmx can
// swap them without a full page reload.
func (s *Server) retryDelivery(w http.ResponseWriter, r *http.Request) {
	alertID, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	channelID, err := pathID(r, "channelID")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	alert, err := s.store.GetAlert(r.Context(), alertID)
	if err != nil {
		s.fail(w, err)
		return
	}
	ch, err := s.store.GetChannel(r.Context(), channelID)
	if err != nil {
		s.fail(w, err)
		return
	}
	attempts, err := s.store.ListDeliveryAttempts(r.Context(), alertID, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := notify.GateRetry(attempts, channelID, *ch, time.Now(), notify.DefaultRetryCooldown); err != nil {
		s.writeDeliveriesFragment(w, r, alertID, err.Error())
		return
	}
	na := notify.Alert{
		ID:        alert.ID,
		MonitorID: alert.MonitorID,
		RuleID:    alert.RuleID,
		EventID:   alert.EventID,
		Payload:   alert.Payload,
		CreatedAt: alert.CreatedAt,
	}
	if m, merr := s.store.GetMonitor(r.Context(), alert.MonitorID); merr == nil && m != nil {
		na.MonitorName = m.Name
	}
	if rule, rerr := s.store.GetRule(r.Context(), alert.RuleID); rerr == nil && rule != nil {
		na.RuleType = rule.Type
	}
	notify.NewDispatcher(s.store, s.factory, s.log).Retry(r.Context(), na, *ch)
	s.writeDeliveriesFragment(w, r, alertID, "")
}

func (s *Server) writeDeliveriesFragment(w http.ResponseWriter, r *http.Request, alertID int64, note string) {
	attempts, err := s.store.ListDeliveryAttempts(r.Context(), alertID, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	channels, err := s.store.ListChannels(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	names := map[int64]string{}
	for _, c := range channels {
		names[c.ID] = c.Name
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if note != "" {
		fmt.Fprintf(w, `<p class="muted">%s</p>`, template.HTMLEscapeString(note))
	}
	if len(attempts) == 0 {
		fmt.Fprint(w, `<p class="muted">No delivery attempts yet.</p>`)
		return
	}
	fmt.Fprint(w, `<table><tr><th>Channel</th><th>Status</th><th>Response</th><th>At</th><th></th></tr>`)
	for _, a := range attempts {
		pillClass := "off"
		if a.Status == "success" {
			pillClass = "on"
		}
		retry := ""
		if a.Status != "success" {
			retry = fmt.Sprintf(
				`<button type="button" hx-post="/alerts/%d/deliveries/%d/retry" hx-target="closest div" hx-swap="innerHTML">Retry</button>`,
				alertID, a.ChannelID)
		}
		fmt.Fprintf(w, `<tr><td>%s</td><td><span class="pill %s">%s</span></td><td><code>%s</code></td><td>%s</td><td>%s</td></tr>`,
			template.HTMLEscapeString(names[a.ChannelID]),
			pillClass, template.HTMLEscapeString(a.Status),
			template.HTMLEscapeString(a.ResponseSnippet),
			formatTime(a.AttemptedAt, tzFromRequest(r)),
			retry)
	}
	fmt.Fprint(w, `</table>`)
}

// alertDetail is the permalink for one alert: monitor, rule, decoded
// event, full payload, and every delivery attempt. Malformed and unknown
// IDs use the same 404 path as the rest of the dashboard.
func (s *Server) alertDetail(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a, err := s.store.GetAlert(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}

	var monitor *store.Monitor
	if m, err := s.store.GetMonitor(r.Context(), a.MonitorID); err == nil {
		monitor = m
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	var rule *store.Rule
	if ru, err := s.store.GetRule(r.Context(), a.RuleID); err == nil {
		rule = ru
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}

	attempts, err := s.store.ListDeliveryAttempts(r.Context(), a.ID, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	channels, err := s.store.ListChannels(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	channelNames := map[int64]string{}
	for _, c := range channels {
		channelNames[c.ID] = c.Name
	}

	fields := payloadFields(a.Payload)
	s.render(w, r, "alert", map[string]any{
		"Title":          fmt.Sprintf("Alert #%d", a.ID),
		"Alert":          a,
		"Monitor":        monitor,
		"Rule":           rule,
		"Attempts":       attempts,
		"ChannelNames":   channelNames,
		"ContractID":     fields.contractID,
		"EventName":      fields.eventName,
		"Ledger":         fields.ledger,
		"LedgerClosedAt": fields.closedAt,
	})
}

type alertPayloadFields struct {
	contractID string
	eventName  string
	ledger     string
	closedAt   time.Time
}

// payloadFields pulls the permalink columns out of the stored event JSON
// without a second schema. Missing keys stay empty rather than failing
// the page — a payload is still worth showing even if a field is absent.
func payloadFields(raw json.RawMessage) alertPayloadFields {
	var out alertPayloadFields
	if len(raw) == 0 {
		return out
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	out.contractID = stringifyPayload(m["contract_id"])
	out.eventName = stringifyPayload(m["event_name"])
	if out.eventName == "" {
		topics, _ := m["topics"].([]any)
		out.eventName = eventNameFrom(m, topics)
	}
	out.ledger = stringifyPayload(m["ledger"])
	if v, ok := m["ledger_closed_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out.closedAt = t
		}
	}
	return out
}

func stringifyPayload(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
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
