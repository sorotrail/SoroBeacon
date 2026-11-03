package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

var (
	errAlertNotFound   = errors.New("alert not found")
	errChannelNotFound = errors.New("channel not found")
)

type bulkDeadLetterRequest struct {
	IDs       []int64 `json:"ids"`
	ChannelID int64   `json:"channel_id"`
	Cause     string  `json:"cause"`
	OlderThan string  `json:"older_than"`
	Limit     int     `json:"limit"`
}

func (s *Server) listDeadLetters(w http.ResponseWriter, r *http.Request) {
	var filter store.DeadLetterFilter
	if q := r.URL.Query().Get("channel_id"); q != "" {
		id, err := strconv.ParseInt(q, 10, 64)
		if err == nil {
			filter.ChannelID = id
		}
	}
	if q := r.URL.Query().Get("alert_id"); q != "" {
		id, err := strconv.ParseInt(q, 10, 64)
		if err == nil {
			filter.AlertID = id
		}
	}
	if q := r.URL.Query().Get("cause"); q != "" {
		filter.Cause = q
	}
	if q := r.URL.Query().Get("older_than"); q != "" {
		if d, err := time.ParseDuration(q); err == nil && d > 0 {
			filter.OlderThan = d
		}
	}
	if q := r.URL.Query().Get("created_before"); q != "" {
		if t, err := time.Parse(time.RFC3339, q); err == nil {
			filter.CreatedBefore = t
		}
	}
	if q := r.URL.Query().Get("created_after"); q != "" {
		if t, err := time.Parse(time.RFC3339, q); err == nil {
			filter.CreatedAfter = t
		}
	}
	if q := r.URL.Query().Get("limit"); q != "" {
		limit, err := strconv.Atoi(q)
		if err == nil && limit > 0 {
			filter.Limit = limit
		}
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 50
	}
	if q := r.URL.Query().Get("after_id"); q != "" {
		id, err := strconv.ParseInt(q, 10, 64)
		if err == nil {
			filter.AfterID = id
		}
	}

	letters, err := s.store.ListDeadLetters(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if letters == nil {
		letters = []store.DeadLetter{}
	}
	writeJSON(w, http.StatusOK, letters)
}

func (s *Server) listDeadLetterGroups(w http.ResponseWriter, r *http.Request) {
	var filter store.DeadLetterFilter
	if q := r.URL.Query().Get("channel_id"); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			filter.ChannelID = id
		}
	}
	if q := r.URL.Query().Get("cause"); q != "" {
		filter.Cause = q
	}
	if q := r.URL.Query().Get("older_than"); q != "" {
		if d, err := time.ParseDuration(q); err == nil && d > 0 {
			filter.OlderThan = d
		}
	}

	groups, err := s.store.ListDeadLetterGroups(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if groups == nil {
		groups = []store.DeadLetterGroup{}
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) discardDeadLetter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "malformed dead letter id")
		return
	}

	dl, err := s.store.GetDeadLetter(r.Context(), id)
	if err != nil {
		if errorsIsNotFound(err) {
			writeErr(w, r, http.StatusNotFound, "dead letter not found")
			return
		}
		s.fail(w, r, err)
		return
	}

	if err := s.store.DeleteDeadLetter(r.Context(), dl.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.metrics != nil {
		ws, _ := workspace.From(r.Context())
		chName := ""
		if ch, err := s.store.GetChannel(r.Context(), dl.ChannelID); err == nil && ch != nil {
			chName = ch.Name
		}
		s.metrics.ClearDeadLetterFor(string(ws), chName)
	}
	writeNoContent(w)
}

func (s *Server) bulkDiscardDeadLetters(w http.ResponseWriter, r *http.Request) {
	var req bulkDeadLetterRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 && req.ChannelID == 0 && req.Cause == "" && req.OlderThan == "" {
		writeValidation(w, r, []FieldError{{Field: "filter", Reason: "at least one filter or id list is required"}})
		return
	}
	var olderThan time.Duration
	if req.OlderThan != "" {
		d, err := time.ParseDuration(req.OlderThan)
		if err != nil {
			writeValidation(w, r, []FieldError{{Field: "older_than", Reason: "invalid duration"}})
			return
		}
		olderThan = d
	}
	f := store.DeadLetterFilter{
		IDs:       req.IDs,
		ChannelID: req.ChannelID,
		Cause:     req.Cause,
		OlderThan: olderThan,
		Limit:     req.Limit,
	}
	discarded, err := s.store.BulkDeleteDeadLetters(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.metrics != nil && discarded > 0 {
		for i := int64(0); i < discarded; i++ {
			s.metrics.ClearDeadLetter()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"discarded": discarded,
	})
}

func (s *Server) redriveOne(ctx context.Context, dl store.DeadLetter) error {
	alert, err := s.store.GetAlert(ctx, dl.AlertID)
	if err != nil {
		if errorsIsNotFound(err) {
			return errAlertNotFound
		}
		return err
	}

	ch, err := s.store.GetChannel(ctx, dl.ChannelID)
	if err != nil {
		if errorsIsNotFound(err) {
			return errChannelNotFound
		}
		return err
	}

	attempts, err := s.store.ListDeliveryAttempts(ctx, alert.ID, "")
	if err != nil {
		return err
	}
	for _, att := range attempts {
		if att.ChannelID == ch.ID && att.Status == store.DeliveryStatusSuccess {
			if deleteErr := s.store.DeleteDeadLetter(ctx, dl.ID); deleteErr != nil {
				return deleteErr
			}
			return notify.ErrAlreadySucceeded
		}
	}

	if !ch.Enabled {
		return notify.ErrChannelDisabled
	}

	notifier, err := s.factory.New(ch.Type, ch.Config)
	if err != nil {
		return err
	}

	attemptCtx, cancel := context.WithTimeout(ctx, ch.TimeoutDuration())
	defer cancel()

	notifyAlert := notifyAlertFromStore(attemptCtx, s.store, *alert)
	err = notifier.Send(attemptCtx, notifyAlert)
	attemptStatus := store.DeliveryStatusSuccess
	snippet := "redrive: ok"
	if err != nil {
		attemptStatus = store.DeliveryStatusFailed
		snippet = "delivery failed"
	}

	_ = s.store.RecordDeliveryAttempt(attemptCtx, &store.DeliveryAttempt{
		AlertID:         alert.ID,
		ChannelID:       ch.ID,
		Status:          attemptStatus,
		ResponseSnippet: snippet,
	})

	if err != nil {
		return errors.New(snippet)
	}

	if deleteErr := s.store.DeleteDeadLetter(attemptCtx, dl.ID); deleteErr != nil {
		return deleteErr
	}
	if s.metrics != nil {
		ws, _ := workspace.From(ctx)
		s.metrics.ClearDeadLetterFor(string(ws), ch.Name)
	}
	return nil
}

// redriveDeadLetter sends one dead-lettered alert again. A successful send
// removes the row, so a repeated request is a 404 rather than a second page.
// A failed send records an attempt and leaves the row for another try.
func (s *Server) redriveDeadLetter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "malformed dead letter id")
		return
	}

	dl, err := s.store.GetDeadLetter(r.Context(), id)
	if err != nil {
		if errorsIsNotFound(err) {
			writeErr(w, r, http.StatusNotFound, "dead letter not found")
			return
		}
		s.fail(w, r, err)
		return
	}

	err = s.redriveOne(r.Context(), *dl)
	if err != nil {
		if errors.Is(err, errAlertNotFound) {
			writeErr(w, r, http.StatusNotFound, "alert not found")
			return
		}
		if errors.Is(err, errChannelNotFound) {
			writeErr(w, r, http.StatusNotFound, "channel not found")
			return
		}
		if errors.Is(err, notify.ErrAlreadySucceeded) || errors.Is(err, notify.ErrChannelDisabled) {
			writeRetryGate(w, r, err)
			return
		}
		writeErr(w, r, http.StatusBadGateway, "redrive delivery failed: "+err.Error())
		return
	}

	writeNoContent(w)
}

func (s *Server) bulkRedriveDeadLetters(w http.ResponseWriter, r *http.Request) {
	var req bulkDeadLetterRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 && req.ChannelID == 0 && req.Cause == "" && req.OlderThan == "" {
		writeValidation(w, r, []FieldError{{Field: "filter", Reason: "at least one filter or id list is required"}})
		return
	}
	var olderThan time.Duration
	if req.OlderThan != "" {
		d, err := time.ParseDuration(req.OlderThan)
		if err != nil {
			writeValidation(w, r, []FieldError{{Field: "older_than", Reason: "invalid duration"}})
			return
		}
		olderThan = d
	}
	limit := req.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	f := store.DeadLetterFilter{
		IDs:       req.IDs,
		ChannelID: req.ChannelID,
		Cause:     req.Cause,
		OlderThan: olderThan,
		Limit:     limit,
	}
	letters, err := s.store.ListDeadLetters(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var succeeded, failed, skipped int
	for i, dl := range letters {
		if r.Context().Err() != nil {
			break
		}
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		err := s.redriveOne(r.Context(), dl)
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, notify.ErrAlreadySucceeded) || errors.Is(err, notify.ErrChannelDisabled):
			skipped++
		default:
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":     len(letters),
		"succeeded": succeeded,
		"failed":    failed,
		"skipped":   skipped,
	})
}

func errorsIsNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
