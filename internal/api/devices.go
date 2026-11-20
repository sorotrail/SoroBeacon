package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// pushChannel loads the channel and requires it to be a push channel, so a
// device token can only be registered where it will be used. The lookup is
// workspace scoped: another tenant's channel is a 404.
func (s *Server) pushChannel(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return 0, false
	}
	if ch.Type != notify.TypePush {
		writeErr(w, r, http.StatusBadRequest, "channel is not a push channel")
		return 0, false
	}
	return id, true
}

// registerPushDevice serves PUT /channels/{id}/devices/{deviceID}. It is both
// registration and token refresh: the same device id with a new token replaces
// the old one, and re-registering a retired device revives it. The token is
// accepted here and never returned by any response.
func (s *Server) registerPushDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pushChannel(w, r)
	if !ok {
		return
	}
	var body struct {
		Token    string `json:"token"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	err := s.store.UpsertPushDevice(r.Context(), id, chi.URLParam(r, "deviceID"), body.Platform, body.Token)
	if errors.Is(err, store.ErrInvalidDevice) {
		writeErr(w, r, http.StatusBadRequest, "device_id and token are required, and platform must be fcm")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// deletePushDevice serves DELETE /channels/{id}/devices/{deviceID}.
func (s *Server) deletePushDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pushChannel(w, r)
	if !ok {
		return
	}
	if err := s.store.DeletePushDevice(r.Context(), id, chi.URLParam(r, "deviceID")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// listPushDevices serves GET /channels/{id}/devices. Tokens are credentials
// and are not part of the response.
func (s *Server) listPushDevices(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pushChannel(w, r)
	if !ok {
		return
	}
	devices, err := s.store.ListPushDevices(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if devices == nil {
		devices = []store.PushDevice{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}
