package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sorobeacon/sorobeacon/internal/notify"
	"github.com/sorobeacon/sorobeacon/internal/store"
)

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
	if req.Name == nil || *req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Type == nil || *req.Type == "" {
		writeErr(w, http.StatusBadRequest, "type is required")
		return
	}
	config := json.RawMessage(`{}`)
	if req.Config != nil {
		config = *req.Config
	}
	// Building the notifier validates the config up front.
	if _, err := s.factory.New(*req.Type, config); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ch := store.Channel{
		Name:    *req.Name,
		Type:    *req.Type,
		Config:  config,
		Enabled: req.Enabled == nil || *req.Enabled,
	}
	if err := s.store.CreateChannel(r.Context(), &ch); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ch)
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListChannels(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if list == nil {
		list = []store.Channel{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) updateChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	var req channelRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		ch.Name = *req.Name
	}
	if req.Type != nil {
		ch.Type = *req.Type
	}
	if req.Config != nil {
		ch.Config = *req.Config
	}
	if req.Enabled != nil {
		ch.Enabled = *req.Enabled
	}
	if _, err := s.factory.New(ch.Type, ch.Config); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateChannel(r.Context(), ch); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteChannel(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testChannel sends a synthetic alert through a channel so users can verify
// its configuration end to end.
func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	notifier, err := s.factory.New(ch.Type, ch.Config)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
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
