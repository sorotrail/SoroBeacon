package api

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// WorkspaceQuotaCurrentUsage reports current live counts against a workspace's
// resource quotas.
type WorkspaceQuotaCurrentUsage struct {
	Monitors      int64 `json:"monitors"`
	Rules         int64 `json:"rules"`
	Channels      int64 `json:"channels"`
	Subscriptions int64 `json:"subscriptions"`
}

// WorkspaceQuotaUsageSummary combines a workspace's configured quota limits,
// current resource counts, and remaining query cost budget.
type WorkspaceQuotaUsageSummary struct {
	store.WorkspaceQuota
	Usage                WorkspaceQuotaCurrentUsage `json:"usage"`
	QueryBudgetRemaining float64                    `json:"query_budget_remaining"`
}

// QuotasResponse is the JSON envelope returned by GET /quotas.
type QuotasResponse struct {
	WorkspaceQuotaUsageSummary
	Workspaces []WorkspaceQuotaUsageSummary `json:"workspaces"`
}

type putQuotaRequest struct {
	WorkspaceID          string   `json:"workspace_id"`
	MaxMonitors          *int64   `json:"max_monitors"`
	MaxRules             *int64   `json:"max_rules"`
	MaxChannels          *int64   `json:"max_channels"`
	MaxSubscriptions     *int64   `json:"max_subscriptions"`
	QueryCostBudget      *float64 `json:"query_cost_budget"`
	QueryCostRefillPerSec *float64 `json:"query_cost_refill_per_sec"`
}

// WithQuotaClock overrides the clock used for query budget refill and deduction in tests.
func (s *Server) WithQuotaClock(clock func() time.Time) *Server {
	s.quotaClock = clock
	return s
}

func (s *Server) quotaNow() time.Time {
	if s.quotaClock != nil {
		return s.quotaClock().UTC()
	}
	return time.Now().UTC()
}

func isQuotaExemptPath(path string) bool {
	if isProbePath(path) {
		return true
	}
	trimmed := strings.TrimPrefix(path, "/api/v1")
	return trimmed == "/quotas" || strings.HasPrefix(trimmed, "/quotas/")
}

func estimateRESTQueryCost(r *http.Request) float64 {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1")
	q := r.URL.Query()

	limit := 0
	if rawLimit := q.Get("limit"); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	switch {
	case trimmed == "/alerts.csv" || trimmed == "/alerts/export":
		eff := limit
		if eff <= 0 || eff > 1000 {
			eff = 500
		}
		return 20.0 + float64(eff)/25.0

	case trimmed == "/stats" || trimmed == "/stats/alerts-daily" || trimmed == "/usage" || strings.HasSuffix(trimmed, "/rules/dry-run"):
		cost := 5.0
		if q.Get("from") != "" || q.Get("to") != "" {
			cost += 2.0
		}
		return cost

	case r.Method == http.MethodGet && (trimmed == "/alerts" || trimmed == "/monitors" || trimmed == "/monitors/trash" ||
		trimmed == "/channels" || trimmed == "/dead-letters" || trimmed == "/dead-letters/groups" || trimmed == "/audit"):
		eff := limit
		if eff <= 0 {
			eff = 50
		}
		if eff > 1000 {
			eff = 1000
		}
		cost := 2.0 + float64(eff)/25.0
		if strings.TrimSpace(q.Get("q")) != "" {
			cost += 2.0
		}
		if q.Get("from") != "" || q.Get("to") != "" {
			cost += 1.0
		}
		return cost

	default:
		return 1.0
	}
}

func reconcileRESTQueryCost(r *http.Request, estimated float64, status int, bytesWritten int64) float64 {
	if status >= 400 {
		return 1.0
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case trimmed == "/alerts.csv" || trimmed == "/alerts/export":
		actual := 10.0 + float64(bytesWritten)/2048.0
		if actual < 5.0 {
			actual = 5.0
		}
		return math.Round(actual*100) / 100
	case r.Method == http.MethodGet && (trimmed == "/alerts" || trimmed == "/monitors" || trimmed == "/channels" || trimmed == "/dead-letters" || trimmed == "/audit"):
		if bytesWritten > 0 && bytesWritten < 256 && estimated > 2.0 {
			return 2.0
		}
		return estimated
	default:
		return estimated
	}
}

type quotaResponseRecorder struct {
	http.ResponseWriter
	status       int
	bytesWritten int64
}

func (r *quotaResponseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *quotaResponseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytesWritten += int64(n)
	return n, err
}

func (r *quotaResponseRecorder) Flush() {
	if fl, ok := r.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (r *quotaResponseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (s *Server) queryBudgetMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isQuotaExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		ws, ok := workspace.From(r.Context())
		if !ok || ws == "" {
			ws = workspace.Default
		}
		now := s.quotaNow()
		estimated := estimateRESTQueryCost(r)

		dec, err := store.SafeConsumeWorkspaceQueryBudget(
			workspace.WithSystem(r.Context()),
			s.store,
			string(ws),
			estimated,
			now,
		)
		if err != nil || !dec.Configured {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("X-Query-Cost-Charged", strconv.FormatFloat(estimated, 'f', -1, 64))
		w.Header().Set("X-Query-Budget-Limit", strconv.FormatFloat(dec.Limit, 'f', -1, 64))
		w.Header().Set("X-Query-Budget-Remaining", strconv.FormatFloat(dec.Remaining, 'f', -1, 64))

		if !dec.Allowed {
			if s.metrics != nil {
				s.metrics.RecordWorkspaceQueryBudgetThrottled(string(ws), "rest")
				s.metrics.SetWorkspaceQueryBudgetRemaining(string(ws), dec.Remaining)
			}
			s.writeQuotaExceeded(w, r, &store.QuotaExceededError{
				WorkspaceID: string(ws),
				Resource:    store.QuotaResourceQueryCostBudget,
				Cost:        estimated,
				Budget:      dec.Limit,
				Remaining:   dec.Remaining,
				RetryAfter:  dec.RetryAfter,
			})
			return
		}

		rec := &quotaResponseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		actual := reconcileRESTQueryCost(r, estimated, rec.status, rec.bytesWritten)
		delta := actual - estimated
		remaining := dec.Remaining
		if math.Abs(delta) >= 0.01 {
			if rem, recErr := store.SafeReconcileWorkspaceQueryBudget(
				workspace.WithSystem(r.Context()),
				s.store,
				string(ws),
				delta,
				now,
			); recErr == nil {
				remaining = rem
			}
		}
		if s.metrics != nil {
			s.metrics.RecordWorkspaceQueryCostCharged(string(ws), "rest", actual)
			s.metrics.SetWorkspaceQueryBudgetRemaining(string(ws), remaining)
		}
	})
}

func (s *Server) writeQuotaExceeded(w http.ResponseWriter, r *http.Request, qErr *store.QuotaExceededError) {
	retrySecs := 1
	if qErr != nil && qErr.RetryAfter > 0 {
		if secs := int(math.Ceil(qErr.RetryAfter.Seconds())); secs > 0 {
			retrySecs = secs
		}
	}
	w.Header().Set("Retry-After", strconv.Itoa(retrySecs))

	msg := "workspace quota exceeded"
	resource := ""
	ws := ""
	if qErr != nil {
		msg = qErr.Error()
		resource = qErr.Resource
		ws = qErr.WorkspaceID
	}
	if s.metrics != nil && resource != "" && resource != store.QuotaResourceQueryCostBudget {
		s.metrics.RecordWorkspaceQuotaRejection(ws, resource)
	}

	body := map[string]any{
		"error":               msg,
		"code":                apierr.CodeQuotaExceeded,
		"request_id":          reqid.From(r),
		"quota":               resource,
		"resource":            resource,
		"workspace_id":        ws,
		"retry_after_seconds": retrySecs,
	}
	if qErr != nil {
		if qErr.Resource == store.QuotaResourceQueryCostBudget {
			body["cost"] = qErr.Cost
			body["budget"] = qErr.Budget
			body["remaining"] = qErr.Remaining
		} else {
			body["limit"] = qErr.Limit
			body["current"] = qErr.Current
			if qErr.Requested > 0 {
				body["requested"] = qErr.Requested
			}
		}
	}
	writeJSON(w, http.StatusTooManyRequests, body)
}

func (s *Server) acquireSubscriptionSlot(ctx context.Context, ws string) (func(), error) {
	if ws == "" {
		ws = string(workspace.Default)
	}
	q, err := store.SafeGetWorkspaceQuota(workspace.WithSystem(ctx), s.store, ws)
	if err != nil {
		return nil, err
	}

	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.activeSubscriptions == nil {
		s.activeSubscriptions = make(map[string]int64)
	}
	current := s.activeSubscriptions[ws]
	if q.MaxSubscriptions > 0 && current >= q.MaxSubscriptions {
		return nil, &store.QuotaExceededError{
			WorkspaceID: ws,
			Resource:    store.QuotaResourceSubscriptions,
			Limit:       q.MaxSubscriptions,
			Current:     current,
			Requested:   1,
			RetryAfter:  5 * time.Second,
		}
	}
	s.activeSubscriptions[ws] = current + 1
	if s.metrics != nil {
		s.metrics.SetWorkspaceQuotaUsage(ws, store.QuotaResourceSubscriptions, float64(current+1))
	}

	released := false
	return func() {
		s.subMu.Lock()
		defer s.subMu.Unlock()
		if released {
			return
		}
		released = true
		next := s.activeSubscriptions[ws] - 1
		if next <= 0 {
			delete(s.activeSubscriptions, ws)
			next = 0
		} else {
			s.activeSubscriptions[ws] = next
		}
		if s.metrics != nil {
			s.metrics.SetWorkspaceQuotaUsage(ws, store.QuotaResourceSubscriptions, float64(next))
		}
	}, nil
}

func (s *Server) activeSubscriptionCount(ws string) int64 {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.activeSubscriptions == nil {
		return 0
	}
	return s.activeSubscriptions[ws]
}

func (s *Server) buildQuotaSummary(ctx context.Context, ws string) (WorkspaceQuotaUsageSummary, error) {
	sysCtx := workspace.WithSystem(ctx)
	q, err := store.SafeGetWorkspaceQuota(sysCtx, s.store, ws)
	if err != nil {
		return WorkspaceQuotaUsageSummary{}, err
	}
	counts, err := store.SafeGetWorkspaceStorageCounts(sysCtx, s.store, ws)
	if err != nil {
		return WorkspaceQuotaUsageSummary{}, err
	}
	subs := s.activeSubscriptionCount(ws)
	remaining := 0.0
	if q.QueryCostBudget > 0 {
		dec, bErr := store.SafeConsumeWorkspaceQueryBudget(sysCtx, s.store, ws, 0, s.quotaNow())
		if bErr == nil {
			remaining = dec.Remaining
		}
	}

	if s.metrics != nil {
		s.metrics.SetWorkspaceQuotaLimit(ws, store.QuotaResourceMonitors, float64(q.MaxMonitors))
		s.metrics.SetWorkspaceQuotaLimit(ws, store.QuotaResourceRules, float64(q.MaxRules))
		s.metrics.SetWorkspaceQuotaLimit(ws, store.QuotaResourceChannels, float64(q.MaxChannels))
		s.metrics.SetWorkspaceQuotaLimit(ws, store.QuotaResourceSubscriptions, float64(q.MaxSubscriptions))
		s.metrics.SetWorkspaceQuotaUsage(ws, store.QuotaResourceMonitors, float64(counts.Monitors))
		s.metrics.SetWorkspaceQuotaUsage(ws, store.QuotaResourceRules, float64(counts.Rules))
		s.metrics.SetWorkspaceQuotaUsage(ws, store.QuotaResourceChannels, float64(counts.Channels))
		s.metrics.SetWorkspaceQuotaUsage(ws, store.QuotaResourceSubscriptions, float64(subs))
		if q.QueryCostBudget > 0 {
			s.metrics.SetWorkspaceQueryBudgetRemaining(ws, remaining)
		}
	}

	return WorkspaceQuotaUsageSummary{
		WorkspaceQuota: q,
		Usage: WorkspaceQuotaCurrentUsage{
			Monitors:      counts.Monitors,
			Rules:         counts.Rules,
			Channels:      counts.Channels,
			Subscriptions: subs,
		},
		QueryBudgetRemaining: remaining,
	}, nil
}

func (s *Server) getQuotas(w http.ResponseWriter, r *http.Request) {
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
			configured, err := store.SafeListWorkspaceQuotas(workspace.WithSystem(r.Context()), s.store)
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

	summaries := make([]WorkspaceQuotaUsageSummary, 0, len(targetWorkspaces))
	for _, ws := range targetWorkspaces {
		sum, err := s.buildQuotaSummary(r.Context(), ws)
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

	writeJSON(w, http.StatusOK, QuotasResponse{
		WorkspaceQuotaUsageSummary: primary,
		Workspaces:                 summaries,
	})
}

func (s *Server) putQuotas(w http.ResponseWriter, r *http.Request) {
	var req putQuotaRequest
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

	sysCtx := workspace.WithSystem(r.Context())
	existing, err := store.SafeGetWorkspaceQuota(sysCtx, s.store, targetWS)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	existing.WorkspaceID = targetWS
	if req.MaxMonitors != nil {
		existing.MaxMonitors = *req.MaxMonitors
	}
	if req.MaxRules != nil {
		existing.MaxRules = *req.MaxRules
	}
	if req.MaxChannels != nil {
		existing.MaxChannels = *req.MaxChannels
	}
	if req.MaxSubscriptions != nil {
		existing.MaxSubscriptions = *req.MaxSubscriptions
	}
	if req.QueryCostBudget != nil {
		existing.QueryCostBudget = *req.QueryCostBudget
	}
	if req.QueryCostRefillPerSec != nil {
		existing.QueryCostRefillPerSec = *req.QueryCostRefillPerSec
	}

	var details []FieldError
	if existing.MaxMonitors < 0 {
		details = append(details, FieldError{Field: "max_monitors", Reason: "must be >= 0"})
	}
	if existing.MaxRules < 0 {
		details = append(details, FieldError{Field: "max_rules", Reason: "must be >= 0"})
	}
	if existing.MaxChannels < 0 {
		details = append(details, FieldError{Field: "max_channels", Reason: "must be >= 0"})
	}
	if existing.MaxSubscriptions < 0 {
		details = append(details, FieldError{Field: "max_subscriptions", Reason: "must be >= 0"})
	}
	if existing.QueryCostBudget < 0 {
		details = append(details, FieldError{Field: "query_cost_budget", Reason: "must be >= 0"})
	}
	if existing.QueryCostRefillPerSec < 0 {
		details = append(details, FieldError{Field: "query_cost_refill_per_sec", Reason: "must be >= 0"})
	}
	if len(details) > 0 {
		writeValidation(w, r, details)
		return
	}

	if _, err := store.SafeSetWorkspaceQuota(sysCtx, s.store, existing); err != nil {
		s.fail(w, r, err)
		return
	}
	summary, err := s.buildQuotaSummary(r.Context(), targetWS)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
