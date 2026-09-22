package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// channelConfigDetails maps a Factory.New error onto envelope fields.
// Unknown types belong on "type"; config problems stay on "config" (or a
// nested path) and must never echo secret values — the constructors
// already avoid that.
func channelConfigDetails(err error) []FieldError {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "unknown channel type") {
		return []FieldError{{Field: "type", Reason: err.Error()}}
	}
	return detailsFromErr("config", err)
}

// channelRequest carries channel writes. Config holds secrets: it is
// accepted on input, validated, stored — and never echoed back (the
// store.Channel JSON marshaller omits it).
type channelRequest struct {
	Name    *string          `json:"name"`
	Type    *string          `json:"type"`
	Config  *json.RawMessage `json:"config"`
	Enabled *bool            `json:"enabled"`
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	var req channelRequest
	if !readJSON(w, r, &req) {
		return
	}
	var details []FieldError
	if req.Name == nil || *req.Name == "" {
		details = append(details, FieldError{Field: "name", Reason: "name is required"})
	}
	if req.Type == nil || *req.Type == "" {
		details = append(details, FieldError{Field: "type", Reason: "type is required"})
	}
	config := json.RawMessage(`{}`)
	if req.Config != nil {
		config = *req.Config
	}
	// Building the notifier validates the config up front. Config holds
	// secrets; the constructors already return messages that name fields
	// without echoing values.
	if req.Type != nil && *req.Type != "" {
		if _, err := s.factory.New(*req.Type, config); err != nil {
			details = append(details, channelConfigDetails(err)...)
		}
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	ch := store.Channel{
		Name:    *req.Name,
		Type:    *req.Type,
		Config:  config,
		Enabled: req.Enabled == nil || *req.Enabled,
	}
	if err := s.store.CreateChannel(r.Context(), &ch); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ch)
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	f, ok := parseListFilter(w, r)
	if !ok {
		return
	}
	list, err := s.store.ListChannelsPage(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []store.Channel{}
	}
	next := ""
	if len(list) == effectivePageLimit(f.Limit) {
		next = strconv.FormatInt(list[len(list)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": list, "next_cursor": next})
}

func (s *Server) getChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) updateChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req channelRequest
	if !readJSON(w, r, &req) {
		return
	}
	var details []FieldError
	if req.Name != nil {
		if *req.Name == "" {
			details = append(details, FieldError{Field: "name", Reason: "name is required"})
		} else {
			ch.Name = *req.Name
		}
	}
	if req.Type != nil {
		if *req.Type == "" {
			details = append(details, FieldError{Field: "type", Reason: "type is required"})
		} else {
			ch.Type = *req.Type
		}
	}
	if req.Config != nil {
		ch.Config = *req.Config
	}
	if req.Enabled != nil {
		ch.Enabled = *req.Enabled
	}
	if _, err := s.factory.New(ch.Type, ch.Config); err != nil {
		details = append(details, channelConfigDetails(err)...)
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	if err := s.store.UpdateChannel(r.Context(), ch); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteChannel(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// testChannel sends a synthetic alert through a channel so users can verify
// its configuration end to end.
func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	notifier, err := s.factory.New(ch.Type, ch.Config)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	testAlert := notify.Alert{
		MonitorName: "Test monitor",
		RuleType:    "test",
		ContractID:  "CCTEST000000000000000000000000000000000000000000000000EXAMPLE",
		EventName:   "sorobeacon_test",
		EventID:     "test-0000000000000000000",
		CreatedAt:   time.Now(),
	}
	if err := notifier.Send(r.Context(), testAlert); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"status": "failed", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
