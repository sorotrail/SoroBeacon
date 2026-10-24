package web

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type DeadLetterViewModel struct {
	ID           int64
	AlertID      int64
	ChannelID    int64
	ChannelName  string
	LastError    string
	AttemptCount int
	LastStatus   int
	CreatedAt    string
}

func (s *Server) listDeadLetters(w http.ResponseWriter, r *http.Request) {
	var filter store.DeadLetterFilter
	if q := r.URL.Query().Get("channel_id"); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			filter.ChannelID = id
		}
	}
	if q := r.URL.Query().Get("alert_id"); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			filter.AlertID = id
		}
	}
	filter.Limit = 100

	letters, err := s.store.ListDeadLetters(r.Context(), filter)
	if err != nil {
		s.renderStatus(w, r, http.StatusInternalServerError, "error", map[string]any{
			"Title": "Error", "Heading": "Something went wrong", "Message": "internal error",
		})
		return
	}

	vms := make([]DeadLetterViewModel, 0, len(letters))
	for _, dl := range letters {
		chName := strconv.FormatInt(dl.ChannelID, 10)
		if ch, err := s.store.GetChannel(r.Context(), dl.ChannelID); err == nil && ch != nil {
			chName = ch.Name
		}
		vms = append(vms, DeadLetterViewModel{
			ID:           dl.ID,
			AlertID:      dl.AlertID,
			ChannelID:    dl.ChannelID,
			ChannelName:  chName,
			LastError:    dl.LastError,
			AttemptCount: dl.AttemptCount,
			LastStatus:   dl.LastStatus,
			CreatedAt:    dl.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
		})
	}

	data := map[string]any{
		"DeadLetters": vms,
	}
	// The page key is the bare name New() parses under: a name that is not
	// in that list resolves to a nil template, which panics the request
	// instead of failing it.
	s.render(w, r, "deadletters", data)
}

func (s *Server) redriveDeadLetter(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.renderStatus(w, r, http.StatusBadRequest, "error", map[string]any{
			"Title": "Bad request", "Heading": "Bad request", "Message": "malformed dead letter id",
		})
		return
	}

	dl, err := s.store.GetDeadLetter(r.Context(), id)
	if err != nil {
		s.renderStatus(w, r, http.StatusNotFound, "error", map[string]any{
			"Title": "Not found", "Heading": "Not found", "Message": "dead letter not found",
		})
		return
	}

	_, err = s.store.GetAlert(r.Context(), dl.AlertID)
	if err != nil {
		s.renderStatus(w, r, http.StatusNotFound, "error", map[string]any{
			"Title": "Not found", "Heading": "Not found", "Message": "alert not found",
		})
		return
	}

	dispatcher := notify.NewDispatcher(s.store, s.factory, s.log)
	if err := dispatcher.RedriveDeadLetter(r.Context(), dl.ID, *dl); err != nil {
		s.renderStatus(w, r, http.StatusBadGateway, "error", map[string]any{
			"Title": "Redrive failed", "Heading": "Redrive failed", "Message": "delivery could not be redriven",
		})
		return
	}

	http.Redirect(w, r, "/dead-letters", http.StatusSeeOther)
}
