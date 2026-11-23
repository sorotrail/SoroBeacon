package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type DeadLetterViewModel struct {
	ID           int64
	AlertID      int64
	ChannelID    int64
	ChannelName  string
	ChannelType  string
	Cause        string
	LastError    string
	AttemptCount int
	LastStatus   int
	CreatedAt    string
	Age          string
}

type DeadLetterGroupViewModel struct {
	ChannelID   int64
	ChannelName string
	ChannelType string
	Cause       string
	Count       int
	OldestAge   string
	NewestAge   string
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Minute)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	days := int(d.Hours()) / 24
	return fmt.Sprintf("%dd ago", days)
}

func (s *Server) listDeadLetters(w http.ResponseWriter, r *http.Request) {
	var filter store.DeadLetterFilter
	selectedChannel := int64(0)
	if q := r.URL.Query().Get("channel_id"); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			filter.ChannelID = id
			selectedChannel = id
		}
	}
	if q := r.URL.Query().Get("alert_id"); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			filter.AlertID = id
		}
	}
	selectedCause := r.URL.Query().Get("cause")
	if selectedCause != "" {
		filter.Cause = selectedCause
	}
	selectedOlderThan := r.URL.Query().Get("older_than")
	if selectedOlderThan != "" {
		if d, err := time.ParseDuration(selectedOlderThan); err == nil && d > 0 {
			filter.OlderThan = d
		}
	}
	filter.Limit = 100

	groups, err := s.store.ListDeadLetterGroups(r.Context(), filter)
	if err != nil {
		s.renderStatus(w, r, http.StatusInternalServerError, "error", map[string]any{
			"Title": "Error", "Heading": "Something went wrong", "Message": "internal error",
		})
		return
	}

	letters, err := s.store.ListDeadLetters(r.Context(), filter)
	if err != nil {
		s.renderStatus(w, r, http.StatusInternalServerError, "error", map[string]any{
			"Title": "Error", "Heading": "Something went wrong", "Message": "internal error",
		})
		return
	}

	channels, err := s.store.ListChannels(r.Context(), false)
	if err != nil {
		channels = nil
	}
	chMap := make(map[int64]store.Channel, len(channels))
	for _, ch := range channels {
		chMap[ch.ID] = ch
	}

	groupVMs := make([]DeadLetterGroupViewModel, 0, len(groups))
	for _, g := range groups {
		chName := g.ChannelName
		chType := g.ChannelType
		if ch, ok := chMap[g.ChannelID]; ok {
			chName = ch.Name
			chType = ch.Type
		}
		groupVMs = append(groupVMs, DeadLetterGroupViewModel{
			ChannelID:   g.ChannelID,
			ChannelName: chName,
			ChannelType: chType,
			Cause:       g.Cause,
			Count:       g.Count,
			OldestAge:   formatAge(g.OldestAt),
			NewestAge:   formatAge(g.NewestAt),
		})
	}

	vms := make([]DeadLetterViewModel, 0, len(letters))
	for _, dl := range letters {
		chName := strconv.FormatInt(dl.ChannelID, 10)
		chType := "unknown"
		if ch, ok := chMap[dl.ChannelID]; ok {
			chName = ch.Name
			chType = ch.Type
		} else if ch, err := s.store.GetChannel(r.Context(), dl.ChannelID); err == nil && ch != nil {
			chName = ch.Name
			chType = ch.Type
		}
		cause := dl.Cause
		if cause == "" {
			cause = store.ClassifyCause(dl.LastStatus, dl.LastError)
		}
		vms = append(vms, DeadLetterViewModel{
			ID:           dl.ID,
			AlertID:      dl.AlertID,
			ChannelID:    dl.ChannelID,
			ChannelName:  chName,
			ChannelType:  chType,
			Cause:        cause,
			LastError:    dl.LastError,
			AttemptCount: dl.AttemptCount,
			LastStatus:   dl.LastStatus,
			CreatedAt:    dl.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
			Age:          formatAge(dl.CreatedAt),
		})
	}

	causes := []string{
		store.DeadLetterCauseRateLimit,
		store.DeadLetterCauseAuthFailure,
		store.DeadLetterCauseClientError,
		store.DeadLetterCauseServerError,
		store.DeadLetterCauseTimeout,
		store.DeadLetterCauseNetwork,
		store.DeadLetterCauseOther,
	}

	data := map[string]any{
		"DeadLetters":       vms,
		"Groups":            groupVMs,
		"Channels":          channels,
		"Causes":            causes,
		"SelectedChannel":   selectedChannel,
		"SelectedCause":     selectedCause,
		"SelectedOlderThan": selectedOlderThan,
	}
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

	s.audit(r, store.AuditActionRedrive, "dead_letter", dl.ID)
	http.Redirect(w, r, "/dead-letters", http.StatusSeeOther)
}

func (s *Server) discardDeadLetter(w http.ResponseWriter, r *http.Request) {
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

	if err := s.store.DeleteDeadLetter(r.Context(), dl.ID); err != nil {
		s.renderStatus(w, r, http.StatusInternalServerError, "error", map[string]any{
			"Title": "Error", "Heading": "Something went wrong", "Message": "internal error",
		})
		return
	}

	s.audit(r, store.AuditActionDiscard, "dead_letter", dl.ID)
	http.Redirect(w, r, "/dead-letters", http.StatusSeeOther)
}

func (s *Server) bulkRedriveDeadLetters(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderStatus(w, r, http.StatusBadRequest, "error", map[string]any{
			"Title": "Bad request", "Heading": "Bad request", "Message": "malformed form",
		})
		return
	}
	var filter store.DeadLetterFilter
	if q := r.FormValue("channel_id"); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			filter.ChannelID = id
		}
	}
	if q := r.FormValue("cause"); q != "" {
		filter.Cause = q
	}
	if q := r.FormValue("older_than"); q != "" {
		if d, err := time.ParseDuration(q); err == nil && d > 0 {
			filter.OlderThan = d
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

	dispatcher := notify.NewDispatcher(s.store, s.factory, s.log)
	for i, dl := range letters {
		if r.Context().Err() != nil {
			break
		}
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		_ = dispatcher.RedriveDeadLetter(r.Context(), dl.ID, dl)
	}

	s.audit(r, store.AuditActionRedrive, "dead_letter", 0, "bulk")
	http.Redirect(w, r, "/dead-letters", http.StatusSeeOther)
}

func (s *Server) bulkDiscardDeadLetters(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderStatus(w, r, http.StatusBadRequest, "error", map[string]any{
			"Title": "Bad request", "Heading": "Bad request", "Message": "malformed form",
		})
		return
	}
	var filter store.DeadLetterFilter
	if q := r.FormValue("channel_id"); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			filter.ChannelID = id
		}
	}
	if q := r.FormValue("cause"); q != "" {
		filter.Cause = q
	}
	if q := r.FormValue("older_than"); q != "" {
		if d, err := time.ParseDuration(q); err == nil && d > 0 {
			filter.OlderThan = d
		}
	}

	_, err := s.store.BulkDeleteDeadLetters(r.Context(), filter)
	if err != nil {
		s.renderStatus(w, r, http.StatusInternalServerError, "error", map[string]any{
			"Title": "Error", "Heading": "Something went wrong", "Message": "internal error",
		})
		return
	}

	s.audit(r, store.AuditActionDiscard, "dead_letter", 0, "bulk")
	http.Redirect(w, r, "/dead-letters", http.StatusSeeOther)
}
