package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// RetentionSummary reports a workspace's configured and effective alert
// retention policy, whether it comes from a workspace override or the
// instance-wide default, and the current visibility/pruning horizon
// (pruned_before).
type RetentionSummary struct {
	store.WorkspaceRetentionPolicy
	AlertRetention        string `json:"alert_retention,omitempty"`
	AlertRetentionSeconds int64  `json:"alert_retention_seconds"`
	DefaultRetention      string `json:"default_retention,omitempty"`
	DefaultRetentionSecs  int64  `json:"default_retention_seconds"`
}

// RetentionResponse is the JSON envelope returned by GET /retention.
type RetentionResponse struct {
	RetentionSummary
	Workspaces []RetentionSummary `json:"workspaces,omitempty"`
}

type putRetentionRequest struct {
	WorkspaceID           string `json:"workspace_id"`
	Retention             string `json:"retention"`
	RetentionSeconds      *int64 `json:"retention_seconds"`
	AlertRetention        string `json:"alert_retention"`
	AlertRetentionSeconds *int64 `json:"alert_retention_seconds"`
	AlertRetentionDays    *int64 `json:"alert_retention_days"`
	ConfirmShorten        bool   `json:"confirm_shorten"`
}

// WithAlertRetention configures the instance-wide default alert retention
// fallback (ALERT_RETENTION) on both the API server and underlying store.
func (s *Server) WithAlertRetention(d time.Duration) *Server {
	if d < 0 {
		d = 0
	}
	s.alertRetention = d
	store.SetInstanceAlertRetention(s.store, d)
	return s
}

func (s *Server) defaultAlertRetention() time.Duration {
	if s.alertRetention > 0 {
		return s.alertRetention
	}
	return store.GetInstanceAlertRetention(s.store)
}

func (s *Server) workspaceRetentionHorizon(ctx context.Context) (*time.Time, string) {
	ws, ok := workspace.From(ctx)
	if !ok || ws == "" {
		ws = workspace.Default
	}
	pol, err := store.SafeGetWorkspaceRetentionPolicy(workspace.WithSystem(ctx), s.store, ws)
	if err != nil {
		return nil, store.RetentionPolicySourceUnset
	}
	if !pol.Configured && pol.EffectiveRetentionSeconds <= 0 && s.defaultAlertRetention() > 0 {
		d := s.defaultAlertRetention().Truncate(time.Second)
		cutoff := time.Now().UTC().Add(-d)
		return &cutoff, store.RetentionPolicySourceInstance
	}
	source := pol.PolicySource
	if source == "" {
		source = store.RetentionPolicySourceUnset
	}
	if pol.EffectiveRetentionSeconds <= 0 {
		return nil, source
	}
	if pol.PrunedBefore != nil {
		return pol.PrunedBefore, source
	}
	cutoff := time.Now().UTC().Add(-pol.EffectiveDuration())
	return &cutoff, source
}

func setRetentionHorizonHeaders(w http.ResponseWriter, prunedBefore *time.Time, policySource string) {
	if policySource != "" {
		w.Header().Set("X-Retention-Policy-Source", policySource)
	}
	if prunedBefore != nil && !prunedBefore.IsZero() {
		w.Header().Set("X-Pruned-Before", prunedBefore.UTC().Format(time.RFC3339))
	}
}

func (s *Server) buildRetentionSummary(ctx context.Context, ws string) (RetentionSummary, error) {
	sysCtx := workspace.WithSystem(ctx)
	pol, err := store.SafeGetWorkspaceRetentionPolicy(sysCtx, s.store, workspace.ID(ws))
	if err != nil {
		return RetentionSummary{}, err
	}
	defDur := s.defaultAlertRetention().Truncate(time.Second)
	defSecs := int64(defDur / time.Second)
	var defStr string
	if defSecs > 0 {
		defStr = (time.Duration(defSecs) * time.Second).String()
	}
	if !pol.Configured && pol.EffectiveRetentionSeconds <= 0 && defSecs > 0 {
		pol.EffectiveRetentionSeconds = defSecs
		pol.EffectiveRetention = defStr
		pol.PolicySource = store.RetentionPolicySourceInstance
		cutoff := time.Now().UTC().Add(-time.Duration(defSecs) * time.Second)
		pol.PrunedBefore = &cutoff
	}
	if s.metrics != nil {
		s.metrics.SetWorkspaceRetentionSeconds(ws, pol.PolicySource, pol.EffectiveDuration())
	}
	return RetentionSummary{
		WorkspaceRetentionPolicy: pol,
		AlertRetention:           pol.EffectiveRetention,
		AlertRetentionSeconds:    pol.EffectiveRetentionSeconds,
		DefaultRetention:         defStr,
		DefaultRetentionSecs:     defSecs,
	}, nil
}

func (s *Server) getRetention(w http.ResponseWriter, r *http.Request) {
	callerWS, ok := workspace.From(r.Context())
	if !ok || callerWS == "" {
		callerWS = workspace.Default
	}
	reqWS := strings.TrimSpace(r.URL.Query().Get("workspace"))

	var targetWorkspaces []string
	if s.isCrossWorkspaceOperator(r) {
		switch {
		case reqWS == "":
			targetWorkspaces = []string{string(callerWS)}
		case reqWS == "*" || strings.EqualFold(reqWS, "all"):
			sysCtx := workspace.WithSystem(r.Context())
			configured, err := store.SafeListWorkspaceRetentionPolicies(sysCtx, s.store)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			seen := make(map[string]bool, len(configured)+1)
			for _, item := range configured {
				seen[item.WorkspaceID] = true
				targetWorkspaces = append(targetWorkspaces, item.WorkspaceID)
			}
			if !seen[string(callerWS)] {
				targetWorkspaces = append(targetWorkspaces, string(callerWS))
			}
		default:
			if !workspace.Valid(reqWS) {
				writeErr(w, r, http.StatusBadRequest, "invalid workspace")
				return
			}
			targetWorkspaces = []string{reqWS}
		}
	} else {
		if reqWS != "" && reqWS != string(callerWS) {
			writeErr(w, r, http.StatusForbidden, "forbidden")
			return
		}
		targetWorkspaces = []string{string(callerWS)}
	}

	summaries := make([]RetentionSummary, 0, len(targetWorkspaces))
	for _, ws := range targetWorkspaces {
		sum, err := s.buildRetentionSummary(r.Context(), ws)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		summaries = append(summaries, sum)
	}

	primary := summaries[0]
	for _, sum := range summaries {
		if sum.WorkspaceID == string(callerWS) {
			primary = sum
			break
		}
	}

	setRetentionHorizonHeaders(w, primary.PrunedBefore, primary.PolicySource)
	writeJSON(w, http.StatusOK, RetentionResponse{
		RetentionSummary: primary,
		Workspaces:       summaries,
	})
}

func (s *Server) writeRetentionShortenConflict(w http.ResponseWriter, r *http.Request, ws string, newSecs int64) {
	current, _ := s.buildRetentionSummary(r.Context(), ws)
	body := map[string]any{
		"error":                     store.ErrRetentionShortenUnconfirmed.Error(),
		"code":                      apierr.CodeConflict,
		"request_id":                reqid.From(r),
		"workspace_id":              ws,
		"current_retention_seconds": current.EffectiveRetentionSeconds,
		"new_retention_seconds":     newSecs,
		"policy_source":             current.PolicySource,
		"confirmation_required":     true,
		"details": []FieldError{
			{
				Field:  "confirm_shorten",
				Reason: "must be true when shortening effective alert retention",
			},
		},
	}
	writeJSON(w, http.StatusConflict, body)
}

func (s *Server) putRetention(w http.ResponseWriter, r *http.Request) {
	var req putRetentionRequest
	if !readJSON(w, r, &req) {
		return
	}

	callerWS, ok := workspace.From(r.Context())
	if !ok || callerWS == "" {
		callerWS = workspace.Default
	}
	targetWS := strings.TrimSpace(req.WorkspaceID)
	if qWS := strings.TrimSpace(r.URL.Query().Get("workspace")); qWS != "" {
		if targetWS != "" && targetWS != qWS {
			writeErr(w, r, http.StatusBadRequest, "conflicting workspace in query and body")
			return
		}
		targetWS = qWS
	}
	if targetWS == "" {
		targetWS = string(callerWS)
	}
	if !workspace.Valid(targetWS) {
		writeErr(w, r, http.StatusBadRequest, "invalid workspace")
		return
	}
	if !s.isCrossWorkspaceOperator(r) && targetWS != string(callerWS) {
		writeErr(w, r, http.StatusForbidden, "forbidden")
		return
	}

	confirmShorten := req.ConfirmShorten
	if qConfirm := strings.TrimSpace(r.URL.Query().Get("confirm_shorten")); qConfirm != "" {
		if strings.EqualFold(qConfirm, "true") || qConfirm == "1" {
			confirmShorten = true
		}
	}

	actor := reqid.From(r)
	if strings.TrimSpace(actor) == "" {
		actor = string(callerWS)
	}

	rawDur := strings.TrimSpace(req.AlertRetention)
	if rawDur == "" {
		rawDur = strings.TrimSpace(req.Retention)
	}
	if strings.EqualFold(rawDur, "default") || strings.EqualFold(rawDur, "inherit") {
		if _, err := store.SafeDeleteWorkspaceRetentionPolicy(r.Context(), s.store, workspace.ID(targetWS), confirmShorten, actor); err != nil {
			if errors.Is(err, store.ErrRetentionShortenUnconfirmed) {
				s.writeRetentionShortenConflict(w, r, targetWS, int64(s.defaultAlertRetention()/time.Second))
				return
			}
			s.fail(w, r, err)
			return
		}
		summary, err := s.buildRetentionSummary(r.Context(), targetWS)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		setRetentionHorizonHeaders(w, summary.PrunedBefore, summary.PolicySource)
		writeJSON(w, http.StatusOK, summary)
		return
	}

	var details []FieldError
	var resolvedSeconds int64
	hasValue := false

	secsPtr := req.AlertRetentionSeconds
	secsField := "alert_retention_seconds"
	if secsPtr == nil && req.RetentionSeconds != nil {
		secsPtr = req.RetentionSeconds
		secsField = "retention_seconds"
	}
	if secsPtr != nil {
		if *secsPtr <= 0 {
			details = append(details, FieldError{Field: secsField, Reason: "must be > 0"})
		} else {
			resolvedSeconds = *secsPtr
			hasValue = true
		}
	}

	if req.AlertRetentionDays != nil {
		if *req.AlertRetentionDays <= 0 {
			details = append(details, FieldError{Field: "alert_retention_days", Reason: "must be > 0"})
		} else {
			daysSeconds := *req.AlertRetentionDays * 86400
			if hasValue && resolvedSeconds != daysSeconds {
				details = append(details, FieldError{Field: "alert_retention_days", Reason: "conflicts with " + secsField})
			} else {
				resolvedSeconds = daysSeconds
				hasValue = true
			}
		}
	}

	if rawDur != "" {
		durField := "alert_retention"
		if strings.TrimSpace(req.AlertRetention) == "" {
			durField = "retention"
		}
		d, err := store.ParseRetentionDuration(rawDur)
		if err != nil {
			details = append(details, FieldError{Field: durField, Reason: err.Error()})
		} else {
			durSeconds := int64(d / time.Second)
			if hasValue && resolvedSeconds != durSeconds {
				details = append(details, FieldError{Field: durField, Reason: "conflicts with " + secsField})
			} else {
				resolvedSeconds = durSeconds
				hasValue = true
			}
		}
	}

	if !hasValue && len(details) == 0 {
		details = append(details, FieldError{
			Field:  "alert_retention_seconds",
			Reason: "alert_retention_seconds or alert_retention is required and must be > 0",
		})
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}

	if _, err := store.SafeSetWorkspaceRetentionPolicy(r.Context(), s.store, store.SetWorkspaceRetentionInput{
		WorkspaceID:       targetWS,
		Retention:         time.Duration(resolvedSeconds) * time.Second,
		ConfirmShorten:    confirmShorten,
		Actor:             actor,
		InstanceRetention: s.defaultAlertRetention(),
	}); err != nil {
		if errors.Is(err, store.ErrRetentionShortenUnconfirmed) {
			s.writeRetentionShortenConflict(w, r, targetWS, resolvedSeconds)
			return
		}
		s.fail(w, r, err)
		return
	}

	summary, err := s.buildRetentionSummary(r.Context(), targetWS)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	setRetentionHorizonHeaders(w, summary.PrunedBefore, summary.PolicySource)
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) deleteRetention(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID    string `json:"workspace_id"`
		ConfirmShorten bool   `json:"confirm_shorten"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				if m := metricsFrom(r.Context()); m != nil {
					m.RecordBodyRejection(rejectionLimitType(r))
				}
				writeErr(w, r, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeErr(w, r, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &req); err != nil {
				writeErr(w, r, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
	}

	callerWS, ok := workspace.From(r.Context())
	if !ok || callerWS == "" {
		callerWS = workspace.Default
	}
	targetWS := strings.TrimSpace(req.WorkspaceID)
	if qWS := strings.TrimSpace(r.URL.Query().Get("workspace")); qWS != "" {
		if targetWS != "" && targetWS != qWS {
			writeErr(w, r, http.StatusBadRequest, "conflicting workspace in query and body")
			return
		}
		targetWS = qWS
	}
	if targetWS == "" {
		targetWS = string(callerWS)
	}
	if !workspace.Valid(targetWS) {
		writeErr(w, r, http.StatusBadRequest, "invalid workspace")
		return
	}
	if !s.isCrossWorkspaceOperator(r) && targetWS != string(callerWS) {
		writeErr(w, r, http.StatusForbidden, "forbidden")
		return
	}

	confirmShorten := req.ConfirmShorten
	if qConfirm := strings.TrimSpace(r.URL.Query().Get("confirm_shorten")); qConfirm != "" {
		if strings.EqualFold(qConfirm, "true") || qConfirm == "1" {
			confirmShorten = true
		}
	}

	actor := reqid.From(r)
	if strings.TrimSpace(actor) == "" {
		actor = string(callerWS)
	}

	if _, err := store.SafeDeleteWorkspaceRetentionPolicy(r.Context(), s.store, workspace.ID(targetWS), confirmShorten, actor); err != nil {
		if errors.Is(err, store.ErrRetentionShortenUnconfirmed) {
			s.writeRetentionShortenConflict(w, r, targetWS, int64(s.defaultAlertRetention()/time.Second))
			return
		}
		s.fail(w, r, err)
		return
	}

	summary, err := s.buildRetentionSummary(r.Context(), targetWS)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	setRetentionHorizonHeaders(w, summary.PrunedBefore, summary.PolicySource)
	writeJSON(w, http.StatusOK, summary)
}
