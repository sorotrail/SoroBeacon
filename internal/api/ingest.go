package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// ingest receives a federated alert from another SoroBeacon instance and
// stores it directly, without re-evaluating rules. The sending instance
// already decided; re-evaluating would duplicate alerts and waste cycles.
//
// Security: the endpoint is behind the same auth middleware as the rest of
// the API. The caller must present a valid bearer token.
//
// The alert's monitor and rule ids are not trusted, and they are not local
// keys either: they belong to the sending instance's database. They are kept
// as the alert's source ids, and the alert itself belongs to no local
// monitor and no local rule, so it cannot be mistaken for one of this
// instance's own matches. It lands in the workspace of the credential that
// delivered it.
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
	sourceMonitorJSON, _ := json.Marshal(fa.Alert.MonitorID)
	payloadMap["_federation_monitor_id"] = json.RawMessage(sourceMonitorJSON)
	sourceRuleJSON, _ := json.Marshal(fa.Alert.RuleID)
	payloadMap["_federation_rule_id"] = json.RawMessage(sourceRuleJSON)
	stampedPayload, _ := json.Marshal(payloadMap)

	severity, ok := severityOf(fa.Alert.Severity)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "invalid severity")
		return
	}

	runbookURL, err := store.ValidateRunbookURL(fa.Alert.RunbookURL)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	responseGuidance, err := store.ValidateResponseGuidance(fa.Alert.ResponseGuidance)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}

	alert := &store.Alert{
		// The payload's monitor and rule ids belong to the sending
		// instance's database. They are kept as the source ids and never
		// written as local keys: this instance has no such rows, and
		// writing them there would either fail the insert or attach the
		// alert to an unrelated local monitor.
		Federated:        true,
		FederationOrigin: fa.Origin,
		SourceMonitorID:  fa.Alert.MonitorID,
		SourceRuleID:     fa.Alert.RuleID,
		EventID:          fa.Alert.EventID,
		Payload:          json.RawMessage(stampedPayload),
		Severity:         severity,
		RunbookURL:       runbookURL,
		ResponseGuidance: responseGuidance,
	}

	outcome, err := s.store.CreateAlert(r.Context(), alert)
	if err != nil {
		s.failIngest(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"outcome":  string(outcome),
		"alert_id": alert.ID,
		"origin":   fa.Origin,
	})
}

// failIngest answers a store failure on /ingest.
//
// Every failure here is about the payload: the alert names a monitor or a rule
// this instance does not have, or a value the database refuses. None of them
// is the receiver's fault, and none of them gets better by being sent again,
// so they are 422 rather than 500. A 500 reads as transient on the sending
// side, which retries three times and then dead-letters a request that can
// never succeed.
func (s *Server) failIngest(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrConstraint) {
		writeErr(w, r, http.StatusUnprocessableEntity, constraintMessage(err))
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, r, http.StatusUnprocessableEntity,
			"the alert references a monitor or rule that does not exist on this instance")
		return
	}
	s.fail(w, r, err)
}

// severityOf maps the sender's severity onto the store's vocabulary.
//
// An unknown value is a malformed payload rather than something to store: the
// database's CHECK constraint would refuse it, and the sender is better
// served by an answer that says so than by a constraint error.
func severityOf(raw string) (store.Severity, bool) {
	switch raw {
	case "":
		return store.SeverityWarning, true
	case string(store.SeverityInfo):
		return store.SeverityInfo, true
	case string(store.SeverityWarning):
		return store.SeverityWarning, true
	case string(store.SeverityCritical):
		return store.SeverityCritical, true
	default:
		return "", false
	}
}
