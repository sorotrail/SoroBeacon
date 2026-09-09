// Package api exposes SoroBeacon's JSON HTTP API (chi router).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sorotrail/sorobeacon/internal/buildinfo"
	"github.com/sorotrail/sorobeacon/internal/notify"
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

type Server struct {
	store    store.Store
	registry *rules.Registry
	factory  *notify.Factory
	rpc      HealthChecker
	log      *slog.Logger
}

// New wires an API server.
func New(st store.Store, reg *rules.Registry, f *notify.Factory, rpc HealthChecker, log *slog.Logger) *Server {
	return &Server{store: st, registry: reg, factory: f, rpc: rpc, log: log}
}

// Routes returns the API router. Mounted under /api/v1 by cmd/sorobeacon.
//
// TODO(contributors): add authentication middleware here; the MVP assumes
// the API is not exposed to untrusted networks.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	r.Route("/monitors", func(r chi.Router) {
		r.Post("/", s.createMonitor)
		r.Get("/", s.listMonitors)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.getMonitor)
			r.Patch("/", s.updateMonitor)
			r.Delete("/", s.deleteMonitor)
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
	r.Get("/health", s.health)
	r.Get("/livez", s.livez)
	r.Get("/readyz", s.readyz)
	r.Get("/version", s.version)
	r.Get("/stats", s.stats)

	return r
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr emits the structured error envelope. The top-level "error"
// string is kept for clients written against the original shape; the
// "request_id" field lets a quoted error be mapped to one request in the
// logs, and "code" gives clients a stable token to branch on.
func writeErr(w http.ResponseWriter, r *http.Request, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error":      msg,
		"code":       http.StatusText(status),
		"request_id": reqid.From(r),
	})
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
	writeJSON(w, status, out)
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
