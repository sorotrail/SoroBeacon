package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// exportWorkspace serves GET /api/v1/workspaces/{id}/export and
// GET /api/v1/workspace/export.
//
// It streams a versioned, self-describing, deterministic export of a single
// tenant workspace's configuration, state, and (when ?include_alerts=true)
// alert history without buffering all rows in memory.
func (s *Server) exportWorkspace(w http.ResponseWriter, r *http.Request) {
	callerWS, ok := workspace.From(r.Context())
	if !ok || callerWS == "" {
		callerWS = workspace.Default
	}

	pathWS := strings.TrimSpace(chi.URLParam(r, "id"))
	queryWS := strings.TrimSpace(r.URL.Query().Get("workspace"))
	if pathWS != "" && queryWS != "" && pathWS != queryWS {
		writeErr(w, r, http.StatusBadRequest, "conflicting workspace in path and query")
		return
	}

	targetWS := pathWS
	if targetWS == "" {
		targetWS = queryWS
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

	includeSecrets, ok := parseExportBoolParam(w, r, "include_secrets", false)
	if !ok {
		return
	}
	if includeSecrets && !s.canExportSecrets(r) {
		s.denyRole(w, r, auth.RoleAdmin)
		return
	}
	includeAlerts, ok := parseExportBoolParam(w, r, "include_alerts", false)
	if !ok {
		return
	}
	pageSize, ok := parseExportPageSizeParam(w, r)
	if !ok {
		return
	}
	format, ok := parseExportFormat(w, r)
	if !ok {
		return
	}

	opts := store.WorkspaceExportOptions{
		IncludeSecrets: includeSecrets,
		IncludeAlerts:  includeAlerts,
		PageSize:       pageSize,
	}

	wsID := workspace.ID(targetWS)
	exportCtx := workspace.With(r.Context(), wsID)

	flusher, _ := w.(http.Flusher)
	fw := &flushCountingWriter{
		w:          w,
		flusher:    flusher,
		flushBytes: 32 << 10,
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Request-ID", reqid.From(r))

	switch format {
	case "ndjson":
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="workspace_%s_export.ndjson"`, targetWS))
		w.WriteHeader(http.StatusOK)
		if err := store.WriteWorkspaceExportNDJSON(exportCtx, fw, s.store, wsID, opts); err != nil {
			s.log.Error("workspace NDJSON export stream failed",
				"request_id", reqid.From(r),
				"workspace", targetWS,
				"err", err,
			)
			return
		}
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="workspace_%s_export.json"`, targetWS))
		w.WriteHeader(http.StatusOK)
		if err := store.WriteWorkspaceExportJSON(exportCtx, fw, s.store, wsID, opts); err != nil {
			s.log.Error("workspace JSON export stream failed",
				"request_id", reqid.From(r),
				"workspace", targetWS,
				"err", err,
			)
			return
		}
	}

	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) canExportSecrets(r *http.Request) bool {
	if s.auth == nil || !s.auth.Enabled() {
		return true
	}
	role, ok := auth.RoleFrom(r.Context())
	if !ok {
		role, _ = s.auth.RoleForRequest(r)
	}
	if !role.AtLeast(auth.RoleAdmin) {
		return false
	}
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && p != nil && p.Restricted() {
		return p.Allowed(auth.ScopeAll)
	}
	return true
}

func parseExportBoolParam(w http.ResponseWriter, r *http.Request, name string, def bool) (bool, bool) {
	raw := strings.TrimSpace(strings.ToLower(r.URL.Query().Get(name)))
	switch raw {
	case "":
		return def, true
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		writeErr(w, r, http.StatusBadRequest, "invalid "+name)
		return false, false
	}
}

func parseExportPageSizeParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("page_size"))
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > store.MaxWorkspaceExportPageSize {
		writeErr(w, r, http.StatusBadRequest, "invalid page_size")
		return 0, false
	}
	return n, true
}

func parseExportFormat(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("format")))
	switch raw {
	case "":
		accept := strings.ToLower(r.Header.Get("Accept"))
		if strings.Contains(accept, "application/x-ndjson") || strings.Contains(accept, "application/ndjson") {
			return "ndjson", true
		}
		return "json", true
	case "json":
		return "json", true
	case "ndjson", "jsonl":
		return "ndjson", true
	default:
		writeErr(w, r, http.StatusBadRequest, "invalid format")
		return "", false
	}
}

type flushCountingWriter struct {
	w          http.ResponseWriter
	flusher    http.Flusher
	flushBytes int
	unflushed  int
}

func (f *flushCountingWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.unflushed += n
	if f.flusher != nil && f.flushBytes > 0 && f.unflushed >= f.flushBytes {
		f.flusher.Flush()
		f.unflushed = 0
	}
	return n, err
}
