package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

type deleteWorkspaceRequest struct {
	DryRun          *bool  `json:"dry_run"`
	Confirm         string `json:"confirm"`
	BatchSize       int    `json:"batch_size"`
	MaxAlertBatches int    `json:"max_alert_batches"`
}

func (s *Server) authorizeWorkspaceTarget(w http.ResponseWriter, r *http.Request) (workspace.ID, bool) {
	rawID := strings.TrimSpace(chi.URLParam(r, "id"))
	if !workspace.Valid(rawID) {
		writeErr(w, r, http.StatusBadRequest, "invalid workspace")
		return "", false
	}
	callerWS, ok := workspace.From(r.Context())
	if !ok || callerWS == "" {
		callerWS = workspace.Default
	}
	if !s.isCrossWorkspaceOperator(r) && rawID != string(callerWS) {
		writeErr(w, r, http.StatusForbidden, "forbidden")
		return "", false
	}
	return workspace.ID(rawID), true
}

func (s *Server) stopWorkspaceActivity(wsID string, channelIDs []int64) (inFlightCancelled, queuedDropped int) {
	if s.dispatcher != nil {
		inFlightCancelled, queuedDropped = s.dispatcher.StopWorkspace(wsID, channelIDs)
	}
	if s.factory != nil {
		s.factory.InvalidateSecretCache()
	}
	s.subMu.Lock()
	if s.activeSubscriptions != nil {
		delete(s.activeSubscriptions, wsID)
	}
	s.subMu.Unlock()
	if s.metrics != nil {
		s.metrics.SetWorkspaceQuotaUsage(wsID, store.QuotaResourceSubscriptions, 0)
	}
	return inFlightCancelled, queuedDropped
}

func (s *Server) getWorkspaceDeletion(w http.ResponseWriter, r *http.Request) {
	targetWS, ok := s.authorizeWorkspaceTarget(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if rawDryRun := strings.TrimSpace(q.Get("dry_run")); rawDryRun != "" {
		parsed, err := strconv.ParseBool(rawDryRun)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid dry_run")
			return
		}
		if parsed {
			res, err := store.SafeDeleteWorkspace(workspace.WithSystem(r.Context()), s.store, targetWS, store.WorkspaceDeletionOptions{
				DryRun: true,
				Actor:  actor(r),
			})
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r)
				return
			}
			if err != nil {
				s.fail(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, res)
			return
		}
	}

	res, err := store.SafeGetWorkspaceDeletion(workspace.WithSystem(r.Context()), s.store, targetWS)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	targetWS, ok := s.authorizeWorkspaceTarget(w, r)
	if !ok {
		return
	}

	var bodyReq deleteWorkspaceRequest
	if r.Body != nil {
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
			if err := json.Unmarshal(data, &bodyReq); err != nil {
				writeErr(w, r, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
	}

	q := r.URL.Query()
	dryRun := false
	if bodyReq.DryRun != nil {
		dryRun = *bodyReq.DryRun
	}
	if rawDryRun := strings.TrimSpace(q.Get("dry_run")); rawDryRun != "" {
		parsed, err := strconv.ParseBool(rawDryRun)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid dry_run")
			return
		}
		dryRun = parsed
	}

	confirm := strings.TrimSpace(bodyReq.Confirm)
	if v := strings.TrimSpace(q.Get("confirm")); v != "" {
		confirm = v
	} else if v := strings.TrimSpace(r.Header.Get("X-Confirm-Workspace")); v != "" && confirm == "" {
		confirm = v
	}

	batchSize := bodyReq.BatchSize
	if v := strings.TrimSpace(q.Get("batch_size")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "invalid batch_size")
			return
		}
		batchSize = n
	} else if batchSize < 0 {
		writeErr(w, r, http.StatusBadRequest, "invalid batch_size")
		return
	}

	maxAlertBatches := bodyReq.MaxAlertBatches
	if v := strings.TrimSpace(q.Get("max_alert_batches")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "invalid max_alert_batches")
			return
		}
		maxAlertBatches = n
	} else if maxAlertBatches < 0 {
		writeErr(w, r, http.StatusBadRequest, "invalid max_alert_batches")
		return
	}

	if !dryRun {
		if targetWS == workspace.Default {
			writeErr(w, r, http.StatusBadRequest, workspace.ErrCannotDeleteDefaultWorkspace.Error())
			return
		}
		if err := workspace.ValidateDeletionConfirmation(targetWS, confirm); err != nil {
			writeValidation(w, r, []FieldError{{
				Field:  "confirm",
				Reason: "must equal workspace id " + strconv.Quote(string(targetWS)) + " to confirm permanent, irreversible deletion (or pass dry_run=true)",
			}})
			return
		}
	}

	act := actor(r)
	var inFlightCancelled, queuedDropped int
	res, err := store.SafeDeleteWorkspace(workspace.WithSystem(r.Context()), s.store, targetWS, store.WorkspaceDeletionOptions{
		DryRun:          dryRun,
		Confirm:         confirm,
		Actor:           act,
		BatchSize:       batchSize,
		MaxAlertBatches: maxAlertBatches,
		OnStop: func(_ context.Context, wsID string, channelIDs []int64) {
			inFlightCancelled, queuedDropped = s.stopWorkspaceActivity(wsID, channelIDs)
		},
	})
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if errors.Is(err, workspace.ErrCannotDeleteDefaultWorkspace) {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	status := http.StatusOK
	if !dryRun {
		res.InFlightCancelled = inFlightCancelled
		res.QueuedDropped = queuedDropped
		if res.Completed && s.dispatcher != nil {
			s.dispatcher.ResumeWorkspace(string(targetWS))
		}
		if !res.Completed {
			status = http.StatusAccepted
		}
		s.recordWorkspaceDeletionAudit(r.Context(), act, res)
	}

	writeJSON(w, status, res)
}

func (s *Server) recordWorkspaceDeletionAudit(ctx context.Context, act string, res *store.WorkspaceDeletionResult) {
	if res == nil {
		return
	}
	if s.log != nil {
		s.log.Info("workspace deletion executed",
			"workspace_id", res.WorkspaceID,
			"actor", act,
			"status", string(res.Status),
			"completed", res.Completed,
			"idempotent_replay", res.IdempotentReplay,
			"batches_completed", res.BatchesCompleted,
			"alerts_deleted", res.Counts.Alerts,
			"monitors_deleted", res.Counts.Monitors,
			"rules_deleted", res.Counts.Rules,
			"channels_deleted", res.Counts.Channels,
			"api_tokens_deleted", res.Counts.APITokens,
			"in_flight_deliveries_cancelled", res.InFlightCancelled,
			"queued_deliveries_dropped", res.QueuedDropped,
		)
	}
	if s.store == nil {
		return
	}
	diffPayload := map[string]any{
		"workspace_id":      res.WorkspaceID,
		"status":            res.Status,
		"completed":         res.Completed,
		"idempotent_replay": res.IdempotentReplay,
		"batches_completed": res.BatchesCompleted,
		"counts":            res.Counts,
	}
	diffBytes, err := json.Marshal(diffPayload)
	if err != nil {
		diffBytes = []byte(`{}`)
	}
	entry := &store.AuditEntry{
		Actor:      act,
		Action:     store.AuditActionDelete,
		TargetType: "workspace",
		TargetID:   0,
		Diff:       diffBytes,
	}
	if err := s.store.CreateAuditEntry(workspace.WithSystem(ctx), entry); err != nil && s.log != nil {
		s.log.Error("write workspace deletion audit entry",
			"workspace_id", res.WorkspaceID,
			"actor", act,
			"err", err,
		)
	}
}
