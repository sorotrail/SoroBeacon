package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// contractIDDetails rejects malformed contract addresses up front: the RPC
// refuses the entire getEvents request if any filter contains one, which
// would stall ingestion for every monitor. Each bad ID is a separate
// detail so a monitor with several typos is not a round-trip per typo.
func contractIDDetails(ids []string) []FieldError {
	var details []FieldError
	for i, id := range ids {
		if !stellar.IsValidContractID(id) {
			details = append(details, FieldError{
				Field:  fmt.Sprintf("contract_ids[%d]", i),
				Reason: "invalid contract id: " + id,
			})
		}
	}
	return details
}

type monitorRequest struct {
	Name        *string   `json:"name"`
	ContractIDs *[]string `json:"contract_ids"`
	Enabled     *bool     `json:"enabled"`
	ChannelIDs  *[]int64  `json:"channel_ids"`
}

func (s *Server) createMonitor(w http.ResponseWriter, r *http.Request) {
	var req monitorRequest
	if !readJSON(w, r, &req) {
		return
	}
	var details []FieldError
	if req.Name == nil || *req.Name == "" {
		details = append(details, FieldError{Field: "name", Reason: "name is required"})
	}
	if req.ContractIDs == nil || len(*req.ContractIDs) == 0 {
		details = append(details, FieldError{Field: "contract_ids", Reason: "contract_ids is required"})
	} else {
		details = append(details, contractIDDetails(*req.ContractIDs)...)
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	m := store.Monitor{
		Name:        *req.Name,
		ContractIDs: *req.ContractIDs,
		Enabled:     req.Enabled == nil || *req.Enabled,
	}
	if err := s.store.CreateMonitor(r.Context(), &m); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ChannelIDs != nil {
		if err := s.store.SetMonitorChannels(r.Context(), m.ID, *req.ChannelIDs); err != nil {
			s.fail(w, r, err)
			return
		}
		m.ChannelIDs = *req.ChannelIDs
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) listMonitors(w http.ResponseWriter, r *http.Request) {
	f, ok := parseListFilter(w, r)
	if !ok {
		return
	}
	monitors, err := s.store.ListMonitorsPage(r.Context(), f)
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

func (s *Server) getMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) updateMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req monitorRequest
	if !readJSON(w, r, &req) {
		return
	}
	var details []FieldError
	if req.Name != nil {
		if *req.Name == "" {
			details = append(details, FieldError{Field: "name", Reason: "name is required"})
		} else {
			m.Name = *req.Name
		}
	}
	if req.ContractIDs != nil {
		if len(*req.ContractIDs) == 0 {
			details = append(details, FieldError{Field: "contract_ids", Reason: "contract_ids is required"})
		} else {
			ids := contractIDDetails(*req.ContractIDs)
			details = append(details, ids...)
			if len(ids) == 0 {
				m.ContractIDs = *req.ContractIDs
			}
		}
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	if req.Enabled != nil {
		m.Enabled = *req.Enabled
	}
	if err := s.store.UpdateMonitor(r.Context(), m); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ChannelIDs != nil {
		if err := s.store.SetMonitorChannels(r.Context(), m.ID, *req.ChannelIDs); err != nil {
			s.fail(w, r, err)
			return
		}
		m.ChannelIDs = *req.ChannelIDs
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteMonitor(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
