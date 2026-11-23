package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// UsageResponse is the JSON envelope returned by GET /usage. It embeds the
// primary WorkspaceUsageSummary at the top level for single-workspace callers
// and carries the full list of returned workspace summaries under Workspaces
// for cross-workspace operator queries.
type UsageResponse struct {
	store.WorkspaceUsageSummary
	Workspaces []store.WorkspaceUsageSummary `json:"workspaces"`
}

func (s *Server) usageMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isProbePath(r.URL.Path) {
			ws, ok := workspace.From(r.Context())
			if !ok || ws == "" {
				ws = workspace.Default
			}
			if s.usageMeter != nil {
				s.usageMeter.RecordAPIRequest(string(ws), 1)
			} else if s.metrics != nil {
				s.metrics.RecordWorkspaceAPIRequest(string(ws), 1)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func parseUsageTimeParam(raw string, isEnd bool) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		t = t.UTC()
		if isEnd {
			t = t.Add(24 * time.Hour)
		}
		return t, nil
	}
	return time.Time{}, errors.New("invalid time")
}

func (s *Server) isCrossWorkspaceOperator(r *http.Request) bool {
	if !s.auth.Enabled() {
		return true
	}
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok || p == nil || p.Restricted() {
		return false
	}
	if p.Role != auth.RoleAdmin {
		return false
	}
	return p.Workspace == "" || p.Workspace == workspace.Default
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	if s.usageMeter != nil {
		_ = s.usageMeter.Flush(r.Context())
	}

	q := r.URL.Query()
	from, err := parseUsageTimeParam(q.Get("from"), false)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid from timestamp")
		return
	}
	to, err := parseUsageTimeParam(q.Get("to"), true)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid to timestamp")
		return
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		writeErr(w, r, http.StatusBadRequest, "to must not precede from")
		return
	}

	callerWS, ok := workspace.From(r.Context())
	if !ok || callerWS == "" {
		callerWS = workspace.Default
	}

	reqWS := strings.TrimSpace(q.Get("workspace"))
	filter := store.WorkspaceUsageFilter{
		From: from,
		To:   to,
	}

	var queryCtx context.Context
	if s.isCrossWorkspaceOperator(r) {
		if reqWS == "" || reqWS == "*" || strings.EqualFold(reqWS, "all") {
			queryCtx = workspace.WithSystem(r.Context())
			filter.WorkspaceID = ""
		} else {
			if !workspace.Valid(reqWS) {
				writeErr(w, r, http.StatusBadRequest, "invalid workspace")
				return
			}
			queryCtx = workspace.WithSystem(r.Context())
			filter.WorkspaceID = reqWS
		}
	} else {
		if reqWS != "" && reqWS != string(callerWS) {
			writeErr(w, r, http.StatusForbidden, "forbidden")
			return
		}
		queryCtx = workspace.With(r.Context(), callerWS)
		filter.WorkspaceID = string(callerWS)
	}

	summaries, err := store.SafeListWorkspaceUsage(queryCtx, s.store, filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(summaries) == 0 {
		ws := filter.WorkspaceID
		if ws == "" {
			ws = string(callerWS)
		}
		var fromPtr, toPtr *time.Time
		if !from.IsZero() {
			f := from
			fromPtr = &f
		}
		if !to.IsZero() {
			t := to
			toPtr = &t
		}
		summaries = []store.WorkspaceUsageSummary{{
			WorkspaceID:   ws,
			From:          fromPtr,
			To:            toPtr,
			Notifications: []store.WorkspaceNotificationUsage{},
			Periods:       []store.WorkspaceUsagePeriod{},
		}}
	}

	if s.usageMeter != nil {
		for _, sum := range summaries {
			s.usageMeter.ObserveStorageCounts(sum.WorkspaceID, sum.StoredRows)
		}
	} else if s.metrics != nil {
		for _, sum := range summaries {
			s.metrics.SetWorkspaceStoredRows(sum.WorkspaceID, "monitors", sum.StoredRows.Monitors)
			s.metrics.SetWorkspaceStoredRows(sum.WorkspaceID, "rules", sum.StoredRows.Rules)
			s.metrics.SetWorkspaceStoredRows(sum.WorkspaceID, "channels", sum.StoredRows.Channels)
			s.metrics.SetWorkspaceStoredRows(sum.WorkspaceID, "alerts", sum.StoredRows.Alerts)
			s.metrics.SetWorkspaceStoredRows(sum.WorkspaceID, "delivery_attempts", sum.StoredRows.DeliveryAttempts)
			s.metrics.SetWorkspaceStoredRows(sum.WorkspaceID, "dead_letters", sum.StoredRows.DeadLetters)
		}
	}

	primary := summaries[0]
	if len(summaries) > 1 {
		for _, sum := range summaries {
			if sum.WorkspaceID == string(callerWS) {
				primary = sum
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, UsageResponse{
		WorkspaceUsageSummary: primary,
		Workspaces:            summaries,
	})
}
