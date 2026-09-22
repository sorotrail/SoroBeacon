// Package api exposes SoroBeacon's JSON HTTP API (chi router).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sorotrail/sorobeacon/internal/buildinfo"
	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/poller"
	"github.com/sorotrail/sorobeacon/internal/reqid"
	"github.com/sorotrail/sorobeacon/internal/rules"
	"github.com/sorotrail/sorobeacon/internal/stellar"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// Server holds the API's dependencies.
// HealthChecker is the dependency the health/readyz probes consult. The
// stellar RPC client satisfies it directly; the SoroTrail source provides
// its own adapter in upstream mode, so probes work in both modes.
type HealthChecker interface {
	GetHealth(ctx context.Context) (*stellar.Health, error)
}

// PositionReader is the poller's race-free snapshot of ingest progress.
// Optional: health/readyz omit poller fields when it is nil or not Ready.
type PositionReader interface {
	Position() poller.Position
}

type Server struct {
	store              store.Store
	registry           *rules.Registry
	factory            *notify.Factory
	rpc                HealthChecker
	log                *slog.Logger
	poller             PositionReader
	readyzLagThreshold uint32
	rateLimit          RateLimitConfig
}

// New wires an API server. Rate limiting stays off until WithRateLimit.
func New(st store.Store, reg *rules.Registry, f *notify.Factory, rpc HealthChecker, log *slog.Logger) *Server {
	return &Server{store: st, registry: reg, factory: f, rpc: rpc, log: log}
}

// WithPoller attaches the ingest-position source used by /health and /readyz.
func (s *Server) WithPoller(p PositionReader) *Server {
	s.poller = p
	return s
}

// WithReadyzLagThreshold fails /readyz when ledger lag exceeds n.
// Zero (the default) leaves the probe unaffected so existing deployments
// cannot start failing without opting in.
func (s *Server) WithReadyzLagThreshold(n uint32) *Server {
	s.readyzLagThreshold = n
	return s
}

// WithRateLimit installs the per-client API limiter. Passing RPS <= 0
// leaves the limiter disabled (the zero-value default).
func (s *Server) WithRateLimit(cfg RateLimitConfig) *Server {
	s.rateLimit = cfg
	return s
}

// Routes returns the API router. Mounted under /api/v1 by cmd/sorobeacon.
//
// TODO(contributors): add authentication middleware here; the MVP assumes
// the API is not exposed to untrusted networks.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(RateLimitMiddleware(s.rateLimit))

	r.Route("/monitors", func(r chi.Router) {
		r.Post("/", s.createMonitor)
		r.Get("/", s.listMonitors)
		r.Post("/bulk", s.bulkMonitors)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.getMonitor)
			r.Patch("/", s.updateMonitor)
			r.Delete("/", s.deleteMonitor)
			r.Post("/duplicate", s.duplicateMonitor)
			r.Post("/rules", s.createRule)
			r.Get("/rules", s.listRules)
			r.Patch("/rules/{ruleID}", s.updateRule)
			r.Delete("/rules/{ruleID}", s.deleteRule)
		})
	})

	r.Route("/channels", func(r chi.Router) {
		r.Post("/", s.createChannel)
		r.Get("/", s.listChannels)
		r.Get("/{id}", s.getChannel)
		r.Patch("/{id}", s.updateChannel)
		r.Delete("/{id}", s.deleteChannel)
		r.Post("/{id}/test", s.testChannel)
	})

	r.Get("/alerts", s.listAlerts)
	r.Get("/alerts/{id}/deliveries", s.listDeliveries)
	r.Post("/alerts/{id}/deliveries/{channelID}/retry", s.retryDelivery)
	r.Get("/health", s.health)
	r.Get("/livez", s.livez)
	r.Get("/readyz", s.readyz)
	r.Get("/version", s.version)
	r.Get("/stats", s.stats)

	return r
}

// --- helpers ---

// parseListFilter reads the shared listing query params (enabled, limit,
// cursor, plus monitors-only q/sort) used by GET /monitors and GET
// /channels so they stay on the same dialect as GET /alerts.
func parseListFilter(w http.ResponseWriter, r *http.Request) (store.ListFilter, bool) {
	q := r.URL.Query()
	f := store.ListFilter{EnabledOnly: q.Get("enabled") == "true"}
	switch q.Get("enabled") {
	case "true":
		t := true
		f.Enabled = &t
	case "false":
		t := false
		f.Enabled = &t
	}
	f.Query = strings.TrimSpace(q.Get("q"))
	if v := q.Get("sort"); v != "" {
		switch v {
		case "name", "id", "created_at":
			f.Sort = v
		default:
			writeErr(w, r, http.StatusBadRequest, "invalid sort")
			return f, false
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, r, http.StatusBadRequest, "invalid limit")
			return f, false
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid cursor")
			return f, false
		}
		f.AfterID = id
	}
	return f, true
}

// effectivePageLimit is the size ListMonitorsPage / ListChannelsPage will
// actually return: a missing or >500 limit becomes 50, matching the store.
func effectivePageLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 50
	}
	return limit
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// FieldError is one entry in the optional "details" array on the error
// envelope. Field is a dotted JSON path (rules[0].params.min_amount);
// Reason is the human-readable failure for that field.
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// writeErr emits the structured error envelope. The top-level "error"
// string is kept for clients written against the original shape; the
// "request_id" field lets a quoted error be mapped to one request in the
// logs, and "code" gives clients a stable token to branch on.
//
// details is purely additive: omitted when empty so existing clients keep
// working. When present, every validation problem in the request is listed
// rather than only the first.
func writeErr(w http.ResponseWriter, r *http.Request, status int, msg string, details ...FieldError) {
	body := map[string]any{
		"error":      msg,
		"code":       http.StatusText(status),
		"request_id": reqid.From(r),
	}
	if len(details) > 0 {
		body["details"] = details
	}
	writeJSON(w, status, body)
}

// writeValidation reports one or more field-level problems as a 400.
// A single detail keeps that field's reason as the top-level error so
// existing clients still see "name is required"; several details share
// the summary "validation failed" and list every field in details.
func writeValidation(w http.ResponseWriter, r *http.Request, details []FieldError) {
	if len(details) == 0 {
		return
	}
	msg := details[0].Reason
	if len(details) > 1 {
		msg = "validation failed"
	}
	writeErr(w, r, http.StatusBadRequest, msg, details...)
}

// detailsFromErr turns a Validate / constructor error into envelope
// details. FieldErrors from the rules registry keep their paths, prefixed
// so nested params show up as params.min_amount rather than a flattened
// string. Anything else is a single detail on prefix.
func detailsFromErr(prefix string, err error) []FieldError {
	if err == nil {
		return nil
	}
	var fields rules.FieldErrors
	if errors.As(err, &fields) && len(fields) > 0 {
		out := make([]FieldError, 0, len(fields))
		for _, d := range fields {
			out = append(out, FieldError{Field: joinPath(prefix, d.Field), Reason: d.Reason})
		}
		return out
	}
	var one rules.FieldError
	if errors.As(err, &one) && (one.Field != "" || one.Reason != "") {
		return []FieldError{{Field: joinPath(prefix, one.Field), Reason: one.Reason}}
	}
	return []FieldError{{Field: prefix, Reason: err.Error()}}
}

func joinPath(prefix, field string) string {
	switch {
	case prefix == "":
		return field
	case field == "":
		return prefix
	default:
		return prefix + "." + field
	}
}

// fail maps store errors to HTTP responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, r, http.StatusNotFound, "not found")
		return
	}
	s.log.Error("api error", "request_id", reqid.From(r), "err", err)
	writeErr(w, r, http.StatusInternalServerError, "internal error")
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, name), 10, 64)
}

// --- health & stats ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status := http.StatusOK
	out := map[string]any{"status": "ok", "db": "ok", "rpc": "ok"}
	if err := s.store.Ping(ctx); err != nil {
		out["status"], out["db"] = "degraded", err.Error()
		status = http.StatusServiceUnavailable
	}
	if h, err := s.rpc.GetHealth(ctx); err != nil {
		out["status"], out["rpc"] = "degraded", err.Error()
		status = http.StatusServiceUnavailable
	} else {
		out["rpc_latest_ledger"] = h.LatestLedger
	}
	s.attachPoller(out)
	writeJSON(w, status, out)
}

// attachPoller adds last processed / chain ledger / lag / last poll time
// when a successful poll has completed. Absent before then — zeros would
// read as "perfectly in sync".
func (s *Server) attachPoller(out map[string]any) {
	if s.poller == nil {
		return
	}
	pos := s.poller.Position()
	if !pos.Ready() {
		return
	}
	out["last_processed_ledger"] = pos.LastProcessedLedger
	out["latest_chain_ledger"] = pos.LatestChainLedger
	out["ledger_lag"] = pos.Lag()
	out["last_successful_poll"] = pos.LastSuccessfulPoll.UTC().Format(time.RFC3339)
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Info{
		Version: buildinfo.Version,
		Commit:  buildinfo.Commit,
		Date:    buildinfo.Date,
	})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.GetStats(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
