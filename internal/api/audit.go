package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// AuditRouteSpec is the audit action and target entity type a matched route produces.
type AuditRouteSpec struct {
	Action     string
	TargetType string
}

// AuditRouteTable maps every administrative mutating REST route ("METHOD /pattern")
// to the audit action and target type it records. TestAuditRouteTableCoversEveryMutatingRoute
// walks the router and fails the build if a mutating route is added without an
// entry in either AuditRouteTable or AuditExemptRESTRoutes.
var AuditRouteTable = auth.NewRouteTable(map[string]AuditRouteSpec{
	// Monitors
	"POST /monitors":                 {Action: store.AuditActionCreate, TargetType: store.AuditTargetMonitor},
	"PATCH /monitors/{id}":           {Action: store.AuditActionUpdate, TargetType: store.AuditTargetMonitor},
	"DELETE /monitors/{id}":          {Action: store.AuditActionDelete, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/soft-delete": {Action: store.AuditActionDelete, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/restore":    {Action: store.AuditActionUpdate, TargetType: store.AuditTargetMonitor},
	"POST /monitors/{id}/duplicate":  {Action: store.AuditActionCreate, TargetType: store.AuditTargetMonitor},

	// Rules
	"POST /monitors/{id}/rules":            {Action: store.AuditActionCreate, TargetType: store.AuditTargetRule},
	"POST /monitors/{id}/rules/bulk":       {Action: store.AuditActionCreate, TargetType: store.AuditTargetRule},
	"PATCH /monitors/{id}/rules/{ruleID}":  {Action: store.AuditActionUpdate, TargetType: store.AuditTargetRule},
	"DELETE /monitors/{id}/rules/{ruleID}": {Action: store.AuditActionDelete, TargetType: store.AuditTargetRule},

	// Escalation policies
	"PUT /monitors/{id}/escalation":    {Action: store.AuditActionUpdate, TargetType: store.AuditTargetEscalationPolicy},
	"DELETE /monitors/{id}/escalation": {Action: store.AuditActionDelete, TargetType: store.AuditTargetEscalationPolicy},

	// Channels & webhook signing keys
	"POST /channels":                         {Action: store.AuditActionCreate, TargetType: store.AuditTargetChannel},
	"PATCH /channels/{id}":                   {Action: store.AuditActionUpdate, TargetType: store.AuditTargetChannel},
	"DELETE /channels/{id}":                  {Action: store.AuditActionDelete, TargetType: store.AuditTargetChannel},
	"POST /channels/{id}/duplicate":          {Action: store.AuditActionCreate, TargetType: store.AuditTargetChannel},
	"POST /channels/{id}/keys/rotate":        {Action: store.AuditActionUpdate, TargetType: store.AuditTargetChannel},
	"POST /channels/{id}/keys/{keyID}/retire": {Action: store.AuditActionUpdate, TargetType: store.AuditTargetChannel},
	"POST /channels/{id}/keys/{keyID}/revoke": {Action: store.AuditActionUpdate, TargetType: store.AuditTargetChannel},

	// Maintenance windows
	"POST /maintenance-windows":        {Action: store.AuditActionCreate, TargetType: store.AuditTargetMaintenanceWindow},
	"PATCH /maintenance-windows/{id}":  {Action: store.AuditActionUpdate, TargetType: store.AuditTargetMaintenanceWindow},
	"DELETE /maintenance-windows/{id}": {Action: store.AuditActionDelete, TargetType: store.AuditTargetMaintenanceWindow},

	// Alert inhibitions
	"POST /inhibitions":                         {Action: store.AuditActionCreate, TargetType: store.AuditTargetInhibition},
	"DELETE /inhibitions/{sourceID}/{targetID}": {Action: store.AuditActionDelete, TargetType: store.AuditTargetInhibition},

	// Monitor templates
	"POST /templates":                       {Action: store.AuditActionCreate, TargetType: store.AuditTargetTemplate},
	"PATCH /templates/{id}":                 {Action: store.AuditActionUpdate, TargetType: store.AuditTargetTemplate},
	"DELETE /templates/{id}":                {Action: store.AuditActionDelete, TargetType: store.AuditTargetTemplate},
	"POST /templates/{id}/instantiate":      {Action: store.AuditActionCreate, TargetType: store.AuditTargetMonitor},
	"POST /templates/{id}/instantiate/bulk": {Action: store.AuditActionCreate, TargetType: store.AuditTargetMonitor},

	// API tokens
	"POST /tokens":              {Action: store.AuditActionCreate, TargetType: store.AuditTargetToken},
	"POST /tokens/{id}/rotate":  {Action: store.AuditActionUpdate, TargetType: store.AuditTargetToken},
	"POST /tokens/{id}/abandon": {Action: store.AuditActionUpdate, TargetType: store.AuditTargetToken},
	"POST /tokens/{id}/revoke":  {Action: store.AuditActionDelete, TargetType: store.AuditTargetToken},

	// Persisted GraphQL operations
	"POST /graphql/operations":        {Action: store.AuditActionCreate, TargetType: store.AuditTargetPersistedOperation},
	"DELETE /graphql/operations/{id}": {Action: store.AuditActionDelete, TargetType: store.AuditTargetPersistedOperation},

	// Dead letters & alert acknowledgements
	"POST /dead-letters/{id}/redrive": {Action: store.AuditActionRedrive, TargetType: store.AuditTargetDeadLetter},
	"POST /dead-letters/{id}/discard": {Action: store.AuditActionDiscard, TargetType: store.AuditTargetDeadLetter},
	"POST /dead-letters/bulk/redrive": {Action: store.AuditActionRedrive, TargetType: store.AuditTargetDeadLetter},
	"POST /dead-letters/bulk/discard": {Action: store.AuditActionDiscard, TargetType: store.AuditTargetDeadLetter},
	"POST /alerts/{id}/acknowledge":   {Action: store.AuditActionUpdate, TargetType: store.AuditTargetAlert},

	// Workspace quotas, retention & workspace deletion
	"PUT /quotas":                  {Action: store.AuditActionUpdate, TargetType: store.AuditTargetQuota},
	"PUT /retention":               {Action: store.AuditActionUpdate, TargetType: store.AuditTargetWorkspaceRetention},
	"DELETE /retention":            {Action: store.AuditActionDelete, TargetType: store.AuditTargetWorkspaceRetention},
	"DELETE /workspaces/{id}":      {Action: store.AuditActionDelete, TargetType: store.AuditTargetWorkspace},
	"POST /workspaces/{id}/delete": {Action: store.AuditActionDelete, TargetType: store.AuditTargetWorkspace},
})

// AuditExemptRESTRoutes documents mutating HTTP routes on the REST router that
// are intentionally excluded from the administrative audit log (operational
// test-sends, read-only dry-runs, data-plane event ingestion, and bulk enable/disable
// toggles). Every entry must carry a non-empty justification.
var AuditExemptRESTRoutes = map[string]string{
	"POST /monitors/bulk":                           "Bulk enable/disable toggle; pinned by TestNonConfigMutationsAreNotAudited.",
	"POST /channels/bulk":                           "Bulk enable/disable toggle for channels.",
	"POST /monitors/import":                         "Batch contract CSV/JSON import helper; pinned by TestNonConfigMutationsAreNotAudited.",
	"POST /monitors/{id}/rules/dry-run":             "Read-only historical rule dry-run simulation; mutates no persistent state.",
	"POST /channels/{id}/test":                      "Operational test notification send; mutates no channel configuration.",
	"POST /alerts/{id}/deliveries/{channelID}/retry": "Operational alert delivery retry; mutates no configuration.",
	"POST /ingest":                                  "Data-plane federated/webhook event ingestion endpoint.",
	"POST /triggers":                                "Data-plane external event trigger ingestion endpoint.",
}

var auditExemptRESTTable = auth.NewRouteTable(AuditExemptRESTRoutes)

// AuditGraphQLMutationTable maps GraphQL Mutation field names to their audit
// specification. The GraphQL schema is currently read-only (Query only); if a
// Mutation field is ever added to internal/api/graphql/schema.graphqls, the
// mechanical coverage test fails until it is registered here or in AuditExemptGraphQLMutations.
var AuditGraphQLMutationTable = map[string]AuditRouteSpec{}

// AuditExemptGraphQLMutations documents GraphQL Mutation fields that are exempt
// from administrative audit logging.
var AuditExemptGraphQLMutations = map[string]string{}

// actor is who a mutating request is recorded as: the credential's label when
// it carries one (a database token does, and the label is chosen so it can be
// shown without leaking the credential), and the request id otherwise — the
// same value the log line and the audit entry carry, so an entry written by an
// unnamed credential still traces back to the request that wrote it.
func actor(r *http.Request) string {
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && p.Name != "" {
		return p.Name
	}
	return reqid.From(r)
}

// actorType returns whether the request principal represents a person (OIDC /
// dashboard session), automation (API token / mTLS client cert), or anonymous.
func actorType(r *http.Request) string {
	if p, ok := auth.PrincipalFromContext(r.Context()); ok {
		return p.EffectiveActorType()
	}
	return store.AuditActorAutomation
}

// auditBodyCapture bounds how much of a create response is kept so the new
// row's id can be read off it. A JSON body is small; anything larger is not a
// shape we need to parse.
const auditBodyCapture = 8 << 10

type auditTraceCtxKey struct{}

type auditTrace struct {
	handled   bool
	principal *auth.Principal
}

func recordAuditTracePrincipal(ctx context.Context, p *auth.Principal) {
	if ctx == nil || p == nil {
		return
	}
	if trace, ok := ctx.Value(auditTraceCtxKey{}).(*auditTrace); ok {
		trace.principal = p
	}
}

type unauthAuditBucket struct {
	tokens float64
	last   time.Time
}

var (
	unauthAuditMu      sync.Mutex
	unauthAuditBuckets = make(map[string]*unauthAuditBucket)
)

const (
	unauthAuditBurst     = 50.0
	unauthAuditRefillRPS = 5.0
)

func allowUnauthenticatedAuditWrite(sourceIP string, now time.Time) bool {
	if sourceIP == "" {
		sourceIP = "unknown"
	}
	unauthAuditMu.Lock()
	defer unauthAuditMu.Unlock()
	if len(unauthAuditBuckets) > 4096 {
		unauthAuditBuckets = make(map[string]*unauthAuditBucket)
	}
	b, ok := unauthAuditBuckets[sourceIP]
	if !ok {
		unauthAuditBuckets[sourceIP] = &unauthAuditBucket{tokens: unauthAuditBurst - 1, last: now}
		return true
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * unauthAuditRefillRPS
		if b.tokens > unauthAuditBurst {
			b.tokens = unauthAuditBurst
		}
		b.last = now
	}
	if b.tokens < 1.0 {
		return false
	}
	b.tokens -= 1.0
	return true
}

// AuditMiddleware appends one entry for every administrative mutation in
// AuditRouteTable (both succeeded and failed attempts). Reads, bulk imports,
// channel test-sends and alert retries are not configuration changes and are
// not recorded. A failed audit write is logged and never fails the request:
// the operator's change already happened and must not be rolled back.
//
// The recorded diff holds only the sanitized field *names* present in the
// request body. Values are discarded immediately — a channel update body
// carries webhook URLs and tokens, which must never reach the log.
func AuditMiddleware(st store.Store, log *slog.Logger) func(http.Handler) http.Handler {
	return AuditMiddlewareWithOptions(st, false, log)
}

// AuditMiddlewareWithOptions configures AuditMiddleware with proxy-header trust
// for source IP extraction.
func AuditMiddlewareWithOptions(st store.Store, trustForwarded bool, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !mutatingMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if trace, ok := r.Context().Value(auditTraceCtxKey{}).(*auditTrace); ok {
				trace.handled = true
			}
			// chi matches the route inside the handler chain, so the route
			// pattern is only populated after next returns; the body must be
			// read (and restored) before the handler consumes it.
			fields, bodyErr := changedFields(r)
			if bodyErr != nil {
				var maxErr *http.MaxBytesError
				if errors.As(bodyErr, &maxErr) {
					writeErr(w, r, http.StatusRequestEntityTooLarge, "request body too large")
					return
				}
			}
			rec := &auditRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// Idempotent replays and rate-limit / circuit-breaker fast-fails do
			// not append duplicate or synthetic administrative audit rows.
			if rec.Header().Get(IdempotencyReplayedHeader) == "true" {
				return
			}
			if rec.status == http.StatusTooManyRequests || rec.status == http.StatusServiceUnavailable {
				return
			}

			spec, ok := auditRoute(r.Method, auditRoutePattern(r))
			if !ok {
				return
			}
			succeeded := rec.status >= 200 && rec.status < 300
			if !succeeded && rec.status < 400 {
				return
			}

			targetID := auditTargetID(r)
			if succeeded && spec.Action == store.AuditActionCreate {
				if createdID := idFromBody(rec.body.Bytes()); createdID != 0 {
					targetID = createdID
				}
			}
			resourceID := auditResourceID(r, rec.body.Bytes(), succeeded)

			diff := json.RawMessage(`{}`)
			diffMap := map[string]any{}
			if len(fields) > 0 {
				diffMap["fields"] = fields
			}
			if succeeded && spec.TargetType == store.AuditTargetRule {
				if v := versionFromBody(rec.body.Bytes()); v > 0 {
					diffMap["version"] = v
				}
			}
			if len(diffMap) > 0 {
				if b, err := json.Marshal(diffMap); err == nil {
					diff = store.SanitizeAuditDiff(spec.TargetType, b)
				}
			}

			outcome := store.AuditOutcomeSucceeded
			reason := ""
			if !succeeded {
				outcome = store.AuditOutcomeFailed
				reason = reasonFromBody(rec.body.Bytes(), rec.status)
			}

			entry := &store.AuditEntry{
				Actor:      actor(r),
				ActorType:  actorType(r),
				Action:     spec.Action,
				TargetType: spec.TargetType,
				TargetID:   targetID,
				ResourceID: resourceID,
				SourceIP:   clientKey(r, trustForwarded),
				Surface:    store.AuditSurfaceREST,
				Outcome:    outcome,
				StatusCode: rec.status,
				Reason:     reason,
				Diff:       diff,
			}
			writeAuditEntrySafe(r.Context(), st, log, entry)
		})
	}
}

// auditRejectedAuthMiddleware records administrative mutation attempts that
// were rejected by AuthMiddleware (401/403) or RoleCheck (403) before reaching
// the inner AuditMiddleware, without reading r.Body prior to authentication.
func (s *Server) auditRejectedAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mutatingMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		trace := &auditTrace{}
		r = r.WithContext(context.WithValue(r.Context(), auditTraceCtxKey{}, trace))
		rec := &auditRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		if trace.handled {
			return
		}
		if rec.status == http.StatusTooManyRequests || rec.status == http.StatusServiceUnavailable {
			return
		}
		if rec.status < 400 {
			return
		}
		path := routePath(r)
		spec, ok := auditRoute(r.Method, path)
		if !ok {
			return
		}

		act := reqid.From(r)
		actType := store.AuditActorAnonymous
		ws := workspace.Default
		p, ok := auth.PrincipalFromContext(r.Context())
		if !ok && trace.principal != nil {
			p = trace.principal
			ok = true
		}
		if ok && p != nil {
			if p.Name != "" {
				act = p.Name
			}
			actType = p.EffectiveActorType()
			if p.Workspace != "" {
				ws = p.Workspace
			}
		}

		srcIP := clientKey(r, s.rateLimit.TrustForwarded)
		// Guard against unauthenticated 401 floods exhausting DB connections or
		// serializing on default-workspace advisory locks.
		if rec.status == http.StatusUnauthorized && !allowUnauthenticatedAuditWrite(srcIP, time.Now()) {
			if s.log != nil {
				s.log.Warn("rate-limited unauthenticated audit log write",
					"source_ip", srcIP,
					"method", r.Method,
					"path", path,
				)
			}
			return
		}

		targetID := auditTargetID(r)
		resourceID := auditResourceID(r, nil, false)
		if targetID == 0 && resourceID == "" {
			targetID, resourceID = extractTargetFromPath(path)
		}

		ctx := workspace.With(r.Context(), ws)
		entry := &store.AuditEntry{
			WorkspaceID: string(ws),
			Actor:       act,
			ActorType:   actType,
			Action:      spec.Action,
			TargetType:  spec.TargetType,
			TargetID:    targetID,
			ResourceID:  resourceID,
			SourceIP:    srcIP,
			Surface:     store.AuditSurfaceREST,
			Outcome:     store.AuditOutcomeFailed,
			StatusCode:  rec.status,
			Reason:      reasonFromBody(rec.body.Bytes(), rec.status),
			Diff:        json.RawMessage(`{}`),
		}
		writeAuditEntrySafe(ctx, s.store, s.log, entry)
	})
}

func writeAuditEntrySafe(ctx context.Context, st store.Store, log *slog.Logger, entry *store.AuditEntry) {
	if st == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			if log != nil {
				log.Error("audit write panicked", "panic", rec)
			}
		}
	}()
	if err := st.CreateAuditEntry(ctx, entry); err != nil {
		if log != nil {
			log.Error("write audit entry",
				"action", entry.Action,
				"target_type", entry.TargetType,
				"target_id", entry.TargetID,
				"err", err)
		}
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

// auditRoute maps a matched route to the administrative mutation it represents.
func auditRoute(method, pattern string) (AuditRouteSpec, bool) {
	p := strings.TrimSuffix(pattern, "/")
	if p == "" {
		return AuditRouteSpec{}, false
	}
	if _, exempt := auditExemptRESTTable.Lookup(method, p); exempt {
		return AuditRouteSpec{}, false
	}
	if spec, ok := AuditRouteTable.Lookup(method, p); ok {
		return spec, true
	}
	// Accept PUT as an alias on PATCH item routes when tests mount PUT directly.
	if method == http.MethodPut {
		if spec, ok := AuditRouteTable.Lookup(http.MethodPatch, p); ok {
			return spec, true
		}
	}
	return AuditRouteSpec{}, false
}

// auditTargetID reads the row id from the matched route's parameters. Rule
// routes name it ruleID; everything else uses id or sourceID.
func auditTargetID(r *http.Request) int64 {
	for _, name := range []string{"ruleID", "id", "sourceID"} {
		if v := chi.URLParam(r, name); v != "" {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil {
				return id
			}
		}
	}
	return 0
}

func auditResourceID(r *http.Request, respBody []byte, succeeded bool) string {
	if keyID := strings.TrimSpace(chi.URLParam(r, "keyID")); keyID != "" {
		return keyID
	}
	if src := strings.TrimSpace(chi.URLParam(r, "sourceID")); src != "" {
		if tgt := strings.TrimSpace(chi.URLParam(r, "targetID")); tgt != "" {
			return src + ":" + tgt
		}
	}
	if rawID := strings.TrimSpace(chi.URLParam(r, "id")); rawID != "" {
		if _, err := strconv.ParseInt(rawID, 10, 64); err != nil {
			return rawID
		}
	}
	if succeeded {
		return stringIDFromBody(respBody)
	}
	return ""
}

func extractTargetFromPath(path string) (int64, string) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var targetID int64
	var resourceID string
	for i := 0; i < len(segs); i++ {
		if id, err := strconv.ParseInt(segs[i], 10, 64); err == nil && id > 0 {
			targetID = id
		} else if i > 0 && (segs[i-1] == "keys" || segs[i-1] == "operations") && segs[i] != "rotate" {
			resourceID = segs[i]
		}
	}
	return targetID, resourceID
}

// idFromBody extracts the numeric "id" a create response returned. A body that
// is not an object with a numeric id yields 0, which is honest rather than
// guessed.
func idFromBody(body []byte) int64 {
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &out)
	return out.ID
}

func stringIDFromBody(body []byte) string {
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.ID)
}

func versionFromBody(body []byte) int {
	var out struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Version
}

func reasonFromBody(body []byte, status int) string {
	var out struct {
		Error string `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &out) == nil && strings.TrimSpace(out.Error) != "" {
		return strings.TrimSpace(out.Error)
	}
	return strings.ToLower(http.StatusText(status))
}

// changedFields returns the sorted top-level field names present in the
// request body. The values are dropped as soon as they are parsed: only the
// fact that a field was present is stored, never its content. The body is
// restored so the handler still sees it.
func changedFields(r *http.Request) ([]string, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytesForAudit))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, nil
	}
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// maxBodyBytesForAudit caps how much of a request body is read to learn its
// field names.
const maxBodyBytesForAudit = 1 << 20

// auditRecorder captures the status and a bounded prefix of the response so
// a create's id or an error envelope's reason can be read without buffering
// the whole body.
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
// action, outcome, actor, actor_type, surface, time range, workspace (for
// cross-workspace operators) and a bounded limit. Entries are newest first.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx := r.Context()
	callerWS, ok := workspace.From(ctx)
	if !ok || callerWS == "" {
		callerWS = workspace.Default
	}

	requestedWS := strings.TrimSpace(q.Get("workspace"))
	if requestedWS == "" {
		requestedWS = strings.TrimSpace(q.Get("workspace_id"))
	}
	isOperator := s.isCrossWorkspaceOperator(r)
	if requestedWS != "" {
		wsID, err := workspace.Parse(requestedWS)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		if wsID != callerWS && !isOperator {
			writeErr(w, r, http.StatusForbidden, "forbidden: cannot query audit records for another workspace")
			return
		}
		if isOperator {
			ctx = workspace.WithSystem(ctx)
		}
	} else if isOperator && strings.EqualFold(strings.TrimSpace(q.Get("all_workspaces")), "true") {
		ctx = workspace.WithSystem(ctx)
	}

	f := store.AuditFilter{
		TargetType: strings.TrimSpace(q.Get("target_type")),
		ResourceID: strings.TrimSpace(q.Get("resource_id")),
		Action:     strings.TrimSpace(q.Get("action")),
		Actor:      strings.TrimSpace(q.Get("actor")),
		ActorType:  strings.TrimSpace(q.Get("actor_type")),
		Surface:    strings.TrimSpace(q.Get("surface")),
		Outcome:    strings.TrimSpace(q.Get("outcome")),
	}
	if requestedWS != "" {
		f.WorkspaceID = requestedWS
	}
	if f.TargetType != "" && !store.ValidAuditTargetType(f.TargetType) {
		writeErr(w, r, http.StatusBadRequest, "invalid target_type")
		return
	}
	if f.Outcome != "" && !store.ValidAuditOutcome(f.Outcome) {
		writeErr(w, r, http.StatusBadRequest, "invalid outcome")
		return
	}
	if f.ActorType != "" && !store.ValidAuditActorType(f.ActorType) {
		writeErr(w, r, http.StatusBadRequest, "invalid actor_type")
		return
	}
	if f.Surface != "" && !store.ValidAuditSurface(f.Surface) {
		writeErr(w, r, http.StatusBadRequest, "invalid surface")
		return
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
		if err != nil || n < 1 || n > 500 {
			writeErr(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			writeErr(w, r, http.StatusBadRequest, "invalid cursor")
			return
		}
		f.AfterID = id
	}
	entries, err := s.store.ListAuditEntries(ctx, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
