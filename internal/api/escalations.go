package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// escalationStepRequest is one step of an escalation policy write. Delay is an
// optional Go duration string ("5m"); omitted or empty means "fire
// immediately". ChannelIDs must name at least one existing channel: a step
// with no destination would silently do nothing.
type escalationStepRequest struct {
	Delay      *string  `json:"delay"`
	ChannelIDs *[]int64 `json:"channel_ids"`
}

type escalationRequest struct {
	Steps *[]escalationStepRequest `json:"steps"`
}

// escalationStepResponse mirrors the request's shape: delay is a duration
// string, not the raw seconds column.
type escalationStepResponse struct {
	Position   int     `json:"position"`
	Delay      string  `json:"delay"`
	ChannelIDs []int64 `json:"channel_ids"`
}

type escalationResponse struct {
	ID        int64                    `json:"id"`
	MonitorID int64                    `json:"monitor_id"`
	Steps     []escalationStepResponse `json:"steps"`
	CreatedAt time.Time                `json:"created_at"`
	UpdatedAt time.Time                `json:"updated_at"`
}

func escalationResponseFrom(pol *store.EscalationPolicy) escalationResponse {
	steps := make([]escalationStepResponse, 0, len(pol.Steps))
	for _, st := range pol.Steps {
		steps = append(steps, escalationStepResponse{
			Position:   st.Position,
			Delay:      (time.Duration(st.DelaySeconds) * time.Second).String(),
			ChannelIDs: st.ChannelIDs,
		})
	}
	return escalationResponse{
		ID:        pol.ID,
		MonitorID: pol.MonitorID,
		Steps:     steps,
		CreatedAt: pol.CreatedAt,
		UpdatedAt: pol.UpdatedAt,
	}
}

func (s *Server) getMonitorEscalation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	pol, err := s.store.GetEscalationPolicyForMonitor(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, escalationResponseFrom(pol))
}

// putMonitorEscalation creates or replaces a monitor's escalation policy. The
// whole policy is sent on every write so an ordered list is edited atomically
// rather than step by step.
func (s *Server) putMonitorEscalation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	var req escalationRequest
	if !readJSON(w, r, &req) {
		return
	}
	steps, details := s.parseEscalationSteps(r, req)
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	pol, err := s.store.SetEscalationPolicy(r.Context(), id, steps)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, escalationResponseFrom(pol))
}

// parseEscalationSteps validates the request and returns the store steps, plus
// any field-level problems. Channel existence is checked here so an unknown id
// is a clear 400 with the offending path instead of a foreign-key 500.
func (s *Server) parseEscalationSteps(r *http.Request, req escalationRequest) ([]store.EscalationStep, []FieldError) {
	var details []FieldError
	if req.Steps == nil || len(*req.Steps) == 0 {
		return nil, []FieldError{{Field: "steps", Reason: "at least one step is required"}}
	}
	steps := make([]store.EscalationStep, 0, len(*req.Steps))
	for i, raw := range *req.Steps {
		prefix := "steps[" + strconv.Itoa(i) + "]"

		var delay time.Duration
		if raw.Delay != nil && *raw.Delay != "" {
			switch d, err := time.ParseDuration(*raw.Delay); {
			case err != nil:
				details = append(details, FieldError{Field: prefix + ".delay", Reason: "invalid duration: " + err.Error()})
			case d < 0:
				details = append(details, FieldError{Field: prefix + ".delay", Reason: "must not be negative"})
			default:
				delay = d
			}
		}

		if raw.ChannelIDs == nil || len(*raw.ChannelIDs) == 0 {
			details = append(details, FieldError{Field: prefix + ".channel_ids", Reason: "at least one channel is required"})
		} else {
			for j, cid := range *raw.ChannelIDs {
				if _, err := s.store.GetChannel(r.Context(), cid); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						details = append(details, FieldError{
							Field:  prefix + ".channel_ids[" + strconv.Itoa(j) + "]",
							Reason: "unknown channel " + strconv.FormatInt(cid, 10),
						})
						continue
					}
					details = append(details, FieldError{
						Field:  prefix + ".channel_ids[" + strconv.Itoa(j) + "]",
						Reason: "could not verify channel " + strconv.FormatInt(cid, 10),
					})
				}
			}
		}

		step := store.EscalationStep{DelaySeconds: int64(delay / time.Second)}
		if raw.ChannelIDs != nil {
			step.ChannelIDs = *raw.ChannelIDs
		}
		steps = append(steps, step)
	}
	return steps, details
}

func (s *Server) deleteMonitorEscalation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteEscalationPolicy(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// acknowledgeAlert serves POST /alerts/{id}/acknowledge. Acknowledging stops
// any escalation attached to the alert, so the on-call engineer who has seen
// the page does not get escalated on top of it.
func (s *Server) acknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.AcknowledgeAlert(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	a, err := s.store.GetAlert(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
