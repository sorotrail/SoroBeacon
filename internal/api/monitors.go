package api

import (
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// validContractIDs rejects malformed contract addresses up front: the RPC
// refuses the entire getEvents request if any filter contains one, which
// would stall ingestion for every monitor.
func validContractIDs(w http.ResponseWriter, r *http.Request, ids []string) bool {
	for _, id := range ids {
		if !stellar.IsValidContractID(id) {
			writeErr(w, r, http.StatusBadRequest, "invalid contract id: "+id)
			return false
		}
	}
	return true
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
	if req.Name == nil || *req.Name == "" {
		writeErr(w, r, http.StatusBadRequest, "name is required")
		return
	}
	if req.ContractIDs == nil || len(*req.ContractIDs) == 0 {
		writeErr(w, r, http.StatusBadRequest, "contract_ids is required")
		return
	}
	if !validContractIDs(w, r, *req.ContractIDs) {
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
	monitors, err := s.store.ListMonitors(r.Context(), r.URL.Query().Get("enabled") == "true")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if monitors == nil {
		monitors = []store.Monitor{}
	}
	writeJSON(w, http.StatusOK, monitors)
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
	if req.Name != nil {
		m.Name = *req.Name
	}
	if req.ContractIDs != nil {
		if !validContractIDs(w, r, *req.ContractIDs) {
			return
		}
		m.ContractIDs = *req.ContractIDs
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
