package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	MinSeverity *string          `json:"min_severity"`
	Timeout     *json.RawMessage `json:"timeout"`
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

// validateConfigLimits bounds channel configuration and template size specifically.
func validateConfigLimits(r *http.Request, config json.RawMessage) []FieldError {
	var details []FieldError
	if len(config) > MaxChannelConfigBytes {
		details = append(details, FieldError{
			Field:  "config",
			Reason: fmt.Sprintf("channel config exceeds maximum size of %d bytes", MaxChannelConfigBytes),
		})
		if m := metricsFrom(r.Context()); m != nil {
			m.RecordBodyRejection("channel_config_size")
		}
	}
	var cfgMap map[string]any
	if err := json.Unmarshal(config, &cfgMap); err == nil {
		if tmpl, ok := cfgMap["template"].(string); ok {
			if len(tmpl) > MaxTemplateBytes {
				details = append(details, FieldError{
					Field:  "config.template",
					Reason: fmt.Sprintf("template exceeds maximum size of %d bytes", MaxTemplateBytes),
				})
				if m := metricsFrom(r.Context()); m != nil {
					m.RecordBodyRejection("template_size")
				}
			}
		}
	}
	return details
}

func timeoutDetail(raw *json.RawMessage) (time.Duration, *FieldError) {
	if raw == nil {
		return 0, nil
	}
	d, err := store.ParseTimeout(*raw)
	if err != nil {
		return 0, &FieldError{Field: "timeout", Reason: err.Error()}
	}
	return d, nil
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
	timeout := store.DefaultChannelTimeout
	if req.Timeout != nil {
		t, terr := timeoutDetail(req.Timeout)
		if terr != nil {
			details = append(details, *terr)
		} else {
			timeout = t
		}
	}
	config := json.RawMessage(`{}`)
	if req.Config != nil {
		config = *req.Config
	}
	details = append(details, validateConfigLimits(r, config)...)
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
		Timeout:             timeout,
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
	if req.Timeout != nil {
		t, terr := timeoutDetail(req.Timeout)
		if terr != nil {
			details = append(details, *terr)
		} else {
			ch.Timeout = t
		}
	}
	if req.Config != nil {
		ch.Config = *req.Config
		details = append(details, validateConfigLimits(r, *req.Config)...)
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
		// A channel referenced by an escalation step must be detached first;
		// a 409 says so instead of leaving a dangling reference behind.
		if errors.Is(err, store.ErrChannelInUse) {
			writeErr(w, r, http.StatusConflict, err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}
	s.factory.InvalidateSecretCache()
	writeNoContent(w)
}

// maxBulkChannels is the documented ceiling on POST /channels/bulk, matching
// the monitor endpoint. There is no "all channels" shorthand on purpose.
const maxBulkChannels = 100

// bulkChannels enables or disables several channels at once, mirroring
// bulkMonitors: an unknown id is reported in unknown_ids and does not stop the
// known ones from applying. Disabling only flips the flag; queued and
// in-flight deliveries sit in their own tables and are not touched.
func (s *Server) bulkChannels(w http.ResponseWriter, r *http.Request) {
	var req bulkMonitorRequest
	if !readJSON(w, r, &req) {
		return
	}
	var details []FieldError
	if len(req.IDs) == 0 {
		details = append(details, FieldError{Field: "ids", Reason: "must not be empty"})
	}
	if len(req.IDs) > maxBulkChannels {
		details = append(details, FieldError{Field: "ids", Reason: "at most 100 channels per request"})
	}
	if req.Enabled == nil {
		details = append(details, FieldError{Field: "enabled", Reason: "enabled is required"})
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}
	updated, unknown, err := s.store.SetChannelsEnabled(r.Context(), req.IDs, *req.Enabled)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if unknown == nil {
		unknown = []int64{}
	}
	s.factory.InvalidateSecretCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":     updated,
		"unknown_ids": unknown,
		"enabled":     *req.Enabled,
	})
}

// duplicateChannel copies a channel under a unique name. The copy carries the
// source's config, credentials included: re-entering a webhook URL or SMTP
// password to change only the room is exactly the toil this endpoint removes,
// and the copy is created disabled so nothing is delivered before review. It
// reads and writes through the store's own paths, so decryption and
// re-encryption never touch a logger, and it has no monitor attachments: a
// duplicate is a new destination, not a second subscriber.
func (s *Server) duplicateChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	src, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	all, err := s.store.ListChannels(r.Context(), false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	names := make([]string, 0, len(all))
	for _, c := range all {
		names = append(names, c.Name)
	}
	dup := store.Channel{
		Name:                store.CopyMonitorName(src.Name, names),
		Type:                src.Type,
		Config:              src.Config,
		Enabled:             false, // never inherit enabled: a duplicate must be reviewed first
		DigestMode:          src.DigestMode,
		DigestWindowSeconds: src.DigestWindowSeconds,
		MinSeverity:         src.MinSeverity,
		Timeout:             src.Timeout,
	}
	if err := s.store.CreateChannel(r.Context(), &dup); err != nil {
		s.fail(w, r, err)
		return
	}
	s.factory.InvalidateSecretCache()
	writeJSON(w, http.StatusCreated, dup)
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
	testCtx, cancel := context.WithTimeout(r.Context(), ch.TimeoutDuration())
	defer cancel()
	if err := notifier.Send(testCtx, testAlert); err != nil {
		s.recordChannelTestHealth(r.Context(), ch.ID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"status": "failed", "error": err.Error()})
		return
	}
	s.recordChannelTestHealth(r.Context(), ch.ID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// recordChannelTestHealth folds a test send's outcome into the channel's
// health, so an operator who has just fixed a channel watches it clear and one
// who tests a still-broken channel sees the count keep climbing. Leaving it
// out would make the reported health disagree with the button the operator
// just pressed.
//
// A bookkeeping failure is logged, not returned: the send is what the caller
// asked about, and health is derived state.
func (s *Server) recordChannelTestHealth(ctx context.Context, channelID int64, sendErr error) {
	u := notify.TestHealthUpdate(sendErr, time.Now())
	if err := s.store.RecordChannelHealth(ctx, channelID, u); err != nil {
		s.log.Error("record channel health", "channel_id", channelID, "err", err)
	}
}

// The windows GET /channels/{id}/stats accepts. The default matches the
// dashboard's delivery-health column, so the endpoint and the channels page
// answer "how is this channel doing?" over the same span unless the caller
// asks otherwise. The maximum is the same 90-day cap
// (store.ClampAlertSeriesDays) the daily-alert chart uses: it bounds every
// query the endpoint can produce, so no window value can ask the database to
// scan unbounded history. Windows above the cap are rejected rather than
// silently shortened — a caller that asked for a year should learn it did not
// get one, not receive quietly different data than it asked for.
const (
	defaultChannelStatsWindow = 24 * time.Hour
	maxChannelStatsWindow     = 90 * 24 * time.Hour
)

// channelStatsResponse is the wire shape of GET /channels/{id}/stats. It
// embeds the store's aggregate and adds only the window echo, so a client can
// tell which window a response describes without the server restating the
// counts. It carries no channel configuration: only counts, rates and
// timestamps leave the handler, exactly as the store type promises.
type channelStatsResponse struct {
	store.ChannelDeliveryStats
	// Window is the effective window as it was requested (trimmed), or the
	// default "24h" when the parameter was omitted, so a client can match
	// the echo against what it asked for.
	Window string `json:"window"`
}

// channelStats serves GET /channels/{id}/stats?window=24h — per-channel
// delivery outcomes over a bounded window, aggregated in the database (see
// store.GetChannelDeliveryStats) rather than by shipping attempts to Go.
//
// A channel with no attempts in the window is a 200 with zero counts, null
// timestamps and no success_rate: "this channel has not been used (lately)"
// is a real answer, distinct from the 404 that says the channel does not
// exist. A malformed, non-positive or oversized window is a 400 before the
// store is consulted, so a bad value costs a parse and a comparison instead
// of a database scan.
func (s *Server) channelStats(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	window := defaultChannelStatsWindow
	// The label echoes the caller's own spelling (trimmed), falling back to
	// the default: "90m" reads back as "90m", not as Duration.String()'s
	// "1h30m0s", so a client can match the echo against what it sent.
	windowLabel := "24h"
	if raw := strings.TrimSpace(r.URL.Query().Get("window")); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			writeErr(w, r, http.StatusBadRequest,
				fmt.Sprintf("invalid window %q (want a Go duration such as \"24h\" or \"90m\")", raw))
			return
		case d <= 0:
			writeErr(w, r, http.StatusBadRequest, "invalid window: must be greater than zero")
			return
		case d > maxChannelStatsWindow:
			writeErr(w, r, http.StatusBadRequest,
				fmt.Sprintf("invalid window: at most %s", maxChannelStatsWindow))
			return
		default:
			window = d
			windowLabel = raw
		}
	}
	// The cutoff is computed once, in UTC, and bound as a parameter: SQL and
	// Go see the same instant, so the window's start cannot drift between the
	// two no matter which clock moved.
	since := time.Now().UTC().Add(-window)
	stats, err := s.store.GetChannelDeliveryStats(r.Context(), id, since)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, channelStatsResponse{ChannelDeliveryStats: stats, Window: windowLabel})
}
