// Package api implements the soft-delete (trash) endpoints for monitors.
package api

import (
	"net/http"
	"strconv"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func (s *Server) softDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.SoftDeleteMonitor(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) restoreMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.RestoreMonitor(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	// Return the restored monitor
	m, err := s.store.GetMonitor(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) listDeletedMonitors(w http.ResponseWriter, r *http.Request) {
	f, ok := parseListFilter(w, r)
	if !ok {
		return
	}
	monitors, err := s.store.ListDeletedMonitors(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if monitors == nil {
		monitors = []store.Monitor{}
	}
	next := ""
	if len(monitors) == effectivePageLimit(f.Limit) {
		next = strconv.FormatInt(monitors[len(monitors)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitors": monitors, "next_cursor": next})
}

