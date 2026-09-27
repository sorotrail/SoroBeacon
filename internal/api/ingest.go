package api

import (
	"encoding/json"
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// ingest receives a federated alert from another SoroBeacon instance and
// stores it directly, without re-evaluating rules. The sending instance
// already decided; re-evaluating would duplicate alerts and waste cycles.
//
// Security: the endpoint is behind the same auth middleware as the rest of
// the API. The caller must present a valid bearer token. The alert's monitor
// and rule IDs are not trusted — the payload is stored as-is with its origin
// tag so the dashboard can display it as a federated alert.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	var fa notify.FederatedAlert
	if !readJSON(w, r, &fa) {
		return
	}

	if fa.Alert.EventID == "" {
		writeErr(w, r, http.StatusBadRequest, "event_id is required")
		return
	}

	if fa.HopCount >= notify.MaxHopCount {
		writeErr(w, r, http.StatusBadRequest, "hop limit reached; dropping alert to prevent loop")
		return
	}

	if fa.Origin == "" {
		writeErr(w, r, http.StatusBadRequest, "origin is required")
		return
	}

	// Stamp the payload with federation metadata so the dashboard can
	// distinguish federated alerts from locally generated ones.
	payload := fa.Alert.Payload
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	var payloadMap map[string]json.RawMessage
	if err := json.Unmarshal(payload, &payloadMap); err != nil {
		payloadMap = map[string]json.RawMessage{}
	}
	payloadMap["_federated"] = json.RawMessage(`true`)
	originJSON, _ := json.Marshal(fa.Origin)
	payloadMap["_federation_origin"] = json.RawMessage(originJSON)
	hopJSON, _ := json.Marshal(fa.HopCount)
	payloadMap["_federation_hops"] = json.RawMessage(hopJSON)
	stampedPayload, _ := json.Marshal(payloadMap)

	alert := &store.Alert{
		MonitorID: fa.Alert.MonitorID,
		RuleID:    fa.Alert.RuleID,
		EventID:   fa.Alert.EventID,
		Payload:   json.RawMessage(stampedPayload),
	}

	outcome, err := s.store.CreateAlert(r.Context(), alert)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"outcome":  string(outcome),
		"alert_id": alert.ID,
		"origin":   fa.Origin,
	})
}
