package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func (s *Server) searches(w http.ResponseWriter, r *http.Request) {
	searches, err := s.store.ListSavedSearches(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	monitors, err := s.store.ListMonitors(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Validate references: a saved search pointing at a deleted monitor
	// degrades by clearing the stale ID rather than erroring the page.
	monitorSet := make(map[int64]bool, len(monitors))
	for _, m := range monitors {
		monitorSet[m.ID] = true
	}
	for i := range searches {
		if searches[i].Filter.MonitorID != 0 && !monitorSet[searches[i].Filter.MonitorID] {
			searches[i].Filter.MonitorID = 0
		}
	}
	s.render(w, r, "searches", map[string]any{
		"Title":    "Saved Searches",
		"Searches": searches,
		"Monitors": monitors,
	})
}

func (s *Server) createSearch(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	var f store.SavedSearchFilter
	if v := r.FormValue("monitor_id"); v != "" {
		f.MonitorID, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := r.FormValue("rule_id"); v != "" {
		f.RuleID, _ = strconv.ParseInt(v, 10, 64)
	}
	f.ContractID = r.FormValue("contract_id")
	f.Sort = r.FormValue("sort")
	if f.Sort == "" {
		f.Sort = "created_at_desc"
	}
	isDefault := r.FormValue("is_default") == "true"

	ss := store.SavedSearch{Name: name, Filter: f, IsDefault: isDefault}
	if err := s.store.CreateSavedSearch(r.Context(), &ss); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/searches", http.StatusSeeOther)
}

func (s *Server) deleteSearch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteSavedSearch(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/searches", http.StatusSeeOther)
}

func (s *Server) setDefaultSearch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.SetDefaultSearch(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/searches", http.StatusSeeOther)
}

func (s *Server) clearDefaultSearch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.ClearDefaultSearch(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/searches", http.StatusSeeOther)
}

// defaultSearchRedirect checks if there is a default saved search and
// redirects to the alerts page with its filters pre-applied. Used when
// the alerts page is loaded without any query parameters.
func (s *Server) defaultSearchRedirect(r *http.Request) string {
	if r.URL.RawQuery != "" {
		return ""
	}
	searches, err := s.store.ListSavedSearches(r.Context())
	if err != nil {
		return ""
	}
	for _, ss := range searches {
		if ss.IsDefault {
			q := ""
			if ss.Filter.MonitorID != 0 {
				q += fmt.Sprintf("&monitor_id=%d", ss.Filter.MonitorID)
			}
			if ss.Filter.RuleID != 0 {
				q += fmt.Sprintf("&rule_id=%d", ss.Filter.RuleID)
			}
			if ss.Filter.ContractID != "" {
				q += "&contract_id=" + ss.Filter.ContractID
			}
			if ss.Filter.Sort != "" {
				q += "&sort=" + ss.Filter.Sort
			}
			if q != "" {
				return "/alerts?" + q[1:]
			}
		}
	}
	return ""
}
