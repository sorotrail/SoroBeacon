package api

import (
	"encoding/json"
	"net/http"
	"slices"
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
	// DigestMode and DigestWindowSeconds opt the channel into batched
	// delivery. Omitted keeps immediate delivery (the pre-digest default).
	DigestMode          *string `json:"digest_mode"`
	DigestWindowSeconds *int64  `json:"digest_window_seconds"`
	// MinSeverity is the minimum alert severity this channel will receive.
	// Empty means no filter (receive all severities).
	MinSeverity *string `json:"min_severity"`
}

// validateDigest checks a channel's digest settings. Empty mode means
// immediate delivery; "window" requires a positive window so a half-filled
// form cannot batch alerts forever.
func validateDigest(mode string, windowSeconds int64) []FieldError {
	switch mode {
	case store.DigestModeOff:
		if windowSeconds < 0 {
			return []FieldError{{Field: "digest_window_seconds", Reason: "must not be negative"}}
		}
		return nil
	case store.DigestModeWindow:
		if windowSeconds <= 0 {
			return []FieldError{{Field: "digest_window_seconds", Reason: "must be greater than 0 when digest_mode is window"}}
		}
		return nil
	default:
		return []FieldError{{Field: "digest_mode", Reason: `must be "" or "window"`}}
	}
}

// validateMinSeverity checks a channel's min_severity setting. Empty means
// no filter; otherwise it must be one of the valid severity values.
func validateMinSeverity(minSeverity string) []FieldError {
	if minSeverity == "" {
		return nil
	}
	if _, ok := store.ParseSeverity(minSeverity); !ok {
		return []FieldError{{Field: "min_severity", Reason: `must be "info", "warning", or "critical"`}}
	}
	return nil
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
	var digestMode string
	var digestWindow int64
	if req.DigestMode != nil {
		digestMode = *req.DigestMode
	}
	if req.DigestWindowSeconds != nil {
		digestWindow = *req.DigestWindowSeconds
	}
	details = append(details, validateDigest(digestMode, digestWindow)...)
	var minSeverity string
	if req.MinSeverity != nil {
		minSeverity = *req.MinSeverity
	}
	details = append(details, validateMinSeverity(minSeverity)...)
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	ch := store.Channel{
		Name:                *req.Name,
		Type:                *req.Type,
		Config:              config,
		Enabled:             req.Enabled == nil || *req.Enabled,
		DigestMode:          digestMode,
		DigestWindowSeconds: digestWindow,
		MinSeverity:         store.Severity(minSeverity),
	}
	if err := s.store.CreateChannel(r.Context(), &ch); err != nil {
		s.fail(w, r, err)
		return
	}
	// A new channel may reference a secret that was corrected or rotated,
	// so drop cached values and resolve fresh on the next delivery.
	s.factory.InvalidateSecretCache()
	writeJSON(w, http.StatusCreated, ch)
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	f, ok := parseListFilter(w, r)
	if !ok {
		return
	}
	// An unknown type would otherwise return an empty list and look like
	// "no channels configured" rather than "that is not a channel type".
	if f.Type != "" && !slices.Contains(s.factory.Types(), f.Type) {
		writeErr(w, r, http.StatusBadRequest, "invalid type")
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
	if req.DigestMode != nil {
		ch.DigestMode = *req.DigestMode
	}
	if req.DigestWindowSeconds != nil {
		ch.DigestWindowSeconds = *req.DigestWindowSeconds
	}
	if req.MinSeverity != nil {
		if *req.MinSeverity == "" {
			ch.MinSeverity = ""
		} else {
			parsed, ok := store.ParseSeverity(*req.MinSeverity)
			if !ok {
				details = append(details, FieldError{Field: "min_severity", Reason: `must be "info", "warning", or "critical"`})
			} else {
				ch.MinSeverity = parsed
			}
		}
	}
	if _, err := s.factory.New(ch.Type, ch.Config); err != nil {
		details = append(details, channelConfigDetails(err)...)
	}
	details = append(details, validateDigest(ch.DigestMode, ch.DigestWindowSeconds)...)
	details = append(details, validateMinSeverity(string(ch.MinSeverity))...)
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	if err := s.store.UpdateChannel(r.Context(), ch); err != nil {
		s.fail(w, r, err)
		return
	}
	s.factory.InvalidateSecretCache()
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
	s.factory.InvalidateSecretCache()
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
