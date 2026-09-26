package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// auditBodyCapture bounds how much of a create response is kept so the new
// row's id can be read off it. A JSON body is small; anything larger is not a
// shape we need to parse.
const auditBodyCapture = 8 << 10

// auditSpec is the entry a matched route should produce.
type auditSpec struct {
	action     string
	targetType string
}

// AuditMiddleware appends one entry for every create, update and delete of a
// monitor, rule or channel that the API performs, whether it came from a JSON
// client or the dashboard's fetch calls. Reads, bulk imports, channel
// test-sends and alert retries are not configuration changes and are not
// recorded. A failed audit write is logged and never fails the request: the
// operator's change already happened and must not be rolled back.
//
// The recorded diff holds only the field *names* present in the request body.
// Values are discarded immediately — a channel update body carries webhook
// URLs and tokens, which must never reach the log.
func AuditMiddleware(st store.Store, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !mutatingMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			// chi matches the route inside the handler chain, so the route
			// pattern is only populated after next returns; the body must be
			// read (and restored) before the handler consumes it.
			fields := changedFields(r)
			rec := &auditRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			spec, ok := auditRoute(r.Method, auditRoutePattern(r))
			if !ok || rec.status < 200 || rec.status >= 300 {
				return
			}
			targetID := auditTargetID(r)
			if spec.action == store.AuditActionCreate {
				targetID = idFromBody(rec.body.Bytes())
			}
			diff := json.RawMessage(`{}`)
			if len(fields) > 0 {
				if b, err := json.Marshal(map[string]any{"fields": fields}); err == nil {
					diff = b
				}
			}
			entry := &store.AuditEntry{
				Actor:      reqid.From(r),
				Action:     spec.action,
				TargetType: spec.targetType,
				TargetID:   targetID,
				Diff:       diff,
			}
			if err := st.CreateAuditEntry(r.Context(), entry); err != nil {
				log.Error("write audit entry",
					"action", spec.action, "target_type", spec.targetType, "target_id", targetID, "err", err)
			}
		})
	}
}

func mutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func auditRoutePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		return rctx.RoutePattern()
	}
	return ""
}

// auditRoute maps a matched route to the mutation it represents. Only the
// single-item monitor, rule and channel routes are listed: POST on a
// collection creates, PATCH/PUT on an item updates, DELETE removes. Special
// sub-routes (`/duplicate` creates a monitor; `/test` and `/bulk` are not
// configuration changes and are deliberately absent).
func auditRoute(method, pattern string) (auditSpec, bool) {
	p := strings.TrimSuffix(pattern, "/")
	switch p {
	case "/monitors":
		if method == http.MethodPost {
			return auditSpec{store.AuditActionCreate, "monitor"}, true
		}
	case "/monitors/{id}":
		switch method {
		case http.MethodPatch, http.MethodPut:
			return auditSpec{store.AuditActionUpdate, "monitor"}, true
		case http.MethodDelete:
			return auditSpec{store.AuditActionDelete, "monitor"}, true
		}
	case "/monitors/{id}/duplicate":
		if method == http.MethodPost {
			return auditSpec{store.AuditActionCreate, "monitor"}, true
		}
	case "/monitors/{id}/rules":
		if method == http.MethodPost {
			return auditSpec{store.AuditActionCreate, "rule"}, true
		}
	case "/monitors/{id}/rules/{ruleID}":
		switch method {
		case http.MethodPatch, http.MethodPut:
			return auditSpec{store.AuditActionUpdate, "rule"}, true
		case http.MethodDelete:
			return auditSpec{store.AuditActionDelete, "rule"}, true
		}
	case "/channels":
		if method == http.MethodPost {
			return auditSpec{store.AuditActionCreate, "channel"}, true
		}
	case "/channels/{id}":
		switch method {
		case http.MethodPatch, http.MethodPut:
			return auditSpec{store.AuditActionUpdate, "channel"}, true
		case http.MethodDelete:
			return auditSpec{store.AuditActionDelete, "channel"}, true
		}
	}
	return auditSpec{}, false
}

// auditTargetID reads the row id from the matched route's parameters. Rule
// routes name it ruleID; everything else uses id.
func auditTargetID(r *http.Request) int64 {
	for _, name := range []string{"id", "ruleID"} {
		if v := chi.URLParam(r, name); v != "" {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil {
				return id
			}
		}
	}
	return 0
}

// idFromBody extracts the "id" a create response returned. A body that is
// not an object with a numeric id yields 0, which is honest rather than
// guessed.
func idFromBody(body []byte) int64 {
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &out)
	return out.ID
}

// changedFields returns the sorted top-level field names present in the
// request body. The values are dropped as soon as they are parsed: only the
// fact that a field was present is stored, never its content. The body is
// restored so the handler still sees it.
func changedFields(r *http.Request) []string {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytesForAudit))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil
	}
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// maxBodyBytesForAudit caps how much of a request body is read to learn its
// field names.
const maxBodyBytesForAudit = 1 << 20

// auditRecorder captures the status and a bounded prefix of the response so
// a create's id can be read without buffering the whole body.
type auditRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (a *auditRecorder) WriteHeader(code int) {
	a.status = code
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditRecorder) Write(b []byte) (int, error) {
	if remaining := auditBodyCapture - a.body.Len(); remaining > 0 {
		if len(b) > remaining {
			a.body.Write(b[:remaining])
		} else {
			a.body.Write(b)
		}
	}
	return a.ResponseWriter.Write(b)
}

// listAudit serves GET /api/v1/audit. Filters mirror the store's: target,
// time range and a bounded limit. Entries are newest first.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditFilter{TargetType: strings.TrimSpace(q.Get("target_type"))}
	if f.TargetType != "" {
		switch f.TargetType {
		case "monitor", "rule", "channel":
		default:
			writeErr(w, r, http.StatusBadRequest, "invalid target_type")
			return
		}
	}
	if v := q.Get("target_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid target_id")
			return
		}
		f.TargetID = id
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid from (want RFC3339)")
			return
		}
		f.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid to (want RFC3339)")
			return
		}
		f.To = t
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		f.Limit = n
	}
	entries, err := s.store.ListAuditEntries(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
