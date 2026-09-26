package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
	// Priority is "low", "normal" or "high". Omitted means normal, so a
	// client that predates priorities creates a middle-tier monitor.
	Priority *string `json:"priority"`
	// Network is the Stellar chain this monitor's contract ids live on.
	// Omitted means the instance's primary network, so a client that predates
	// multi-network keeps working — and keeps meaning the chain it already
	// polled.
	Network *string `json:"network"`
}

// networkDetail validates an optional network against the chains this instance
// polls. It rejects an unknown name rather than defaulting it: a monitor aimed
// at a chain nobody configured would sit there matching nothing forever, which
// is indistinguishable from a contract that never emits events.
func (s *Server) networkDetail(raw *string) (string, *FieldError) {
	if raw == nil {
		return s.defaultNetwork(), nil
	}
	n := strings.ToLower(strings.TrimSpace(*raw))
	if n == "" {
		return "", &FieldError{Field: "network", Reason: "network must not be empty; omit it to use the primary network"}
	}
	// An instance with no list is the single-network shape: its poller is not
	// scoped to a name, so accepting one would create a monitor nobody reads.
	// Naming the unset variable is the fix, and saying so beats a list of
	// nothing.
	if len(s.networkNames) == 0 {
		return "", &FieldError{
			Field:  "network",
			Reason: "this instance polls one unnamed network; set NETWORKS to poll by name, or omit network",
		}
	}
	for _, cfg := range s.networkNames {
		if cfg == n {
			return n, nil
		}
	}
	return "", &FieldError{
		Field:  "network",
		Reason: fmt.Sprintf("this instance does not poll %q (configured networks: %s)", n, strings.Join(s.networkNames, ", ")),
	}
}

// priorityDetail validates an optional priority, returning a FieldError for an
// unknown tier and the parsed value otherwise. Rejecting unknown values keeps
// the scheduler's tier set closed rather than silently defaulting a typo.
func priorityDetail(raw *string) (store.Priority, *FieldError) {
	if raw == nil {
		return "", nil
	}
	p, ok := store.ParsePriority(*raw)
	if !ok {
		return "", &FieldError{Field: "priority", Reason: `must be one of "low", "normal", "high"`}
	}
	return p, nil
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
	priority, perr := priorityDetail(req.Priority)
	if perr != nil {
		details = append(details, *perr)
	}
	network, nerr := s.networkDetail(req.Network)
	if nerr != nil {
		details = append(details, *nerr)
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	m := store.Monitor{
		Name:        *req.Name,
		ContractIDs: *req.ContractIDs,
		Enabled:     req.Enabled == nil || *req.Enabled,
		Priority:    priority.Normalized(),
		Network:     network,
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
	if priority, perr := priorityDetail(req.Priority); perr != nil {
		details = append(details, *perr)
	} else if req.Priority != nil {
		m.Priority = priority
	}
	// A monitor's network is fixed at creation. Its contract ids only exist on
	// the chain they were deployed to, so re-pointing the monitor at another
	// network would mean "watch these ids where they resolve to something
	// else, or nothing" — and the alerts already stored would keep the old
	// label, so the history and the subscription would disagree about which
	// chain they describe. Recreating the monitor is the honest move.
	if req.Network != nil && !strings.EqualFold(strings.TrimSpace(*req.Network), m.Network) {
		details = append(details, FieldError{
			Field:  "network",
			Reason: fmt.Sprintf("a monitor's network is fixed (%q): delete and recreate the monitor to move it to another chain", m.Network),
		})
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

// maxBulkMonitors is the documented ceiling on POST /monitors/bulk. A
// planned pause should never need more than this in one shot; a larger
// body is almost certainly an unbounded script. There is no "all
// monitors" shorthand on purpose — an accidental global disable is
// exactly the failure this endpoint must not enable.
const maxBulkMonitors = 100

type bulkMonitorRequest struct {
	IDs     []int64 `json:"ids"`
	Enabled *bool   `json:"enabled"`
}

func (s *Server) bulkMonitors(w http.ResponseWriter, r *http.Request) {
	var req bulkMonitorRequest
	if !readJSON(w, r, &req) {
		return
	}
	var details []FieldError
	if len(req.IDs) == 0 {
		details = append(details, FieldError{Field: "ids", Reason: "must not be empty"})
	}
	if len(req.IDs) > maxBulkMonitors {
		details = append(details, FieldError{Field: "ids", Reason: "at most 100 monitors per request"})
	}
	if req.Enabled == nil {
		details = append(details, FieldError{Field: "enabled", Reason: "enabled is required"})
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	updated, unknown, err := s.store.SetMonitorsEnabled(r.Context(), req.IDs, *req.Enabled)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if unknown == nil {
		unknown = []int64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":     updated,
		"unknown_ids": unknown,
		"enabled":     *req.Enabled,
	})
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
	writeNoContent(w)
}

func (s *Server) duplicateMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	m, err := s.store.DuplicateMonitor(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}
