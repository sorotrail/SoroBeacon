package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// probeTimeout bounds each readiness check, so one hanging dependency
// cannot hang the probe itself.
const probeTimeout = 3 * time.Second

// livez answers "is the process alive". It checks nothing: a process whose
// database and RPC are both down is still alive, and a liveness probe that
// failed on dependencies would cause restart loops instead of traffic
// removal. Kubernetes should wire this to livenessProbe.
func (s *Server) livez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "alive"})
}

// readyz answers "can this instance do useful work right now". It checks
// the two things every request path depends on — the database and the RPC
// endpoint — concurrently and with per-dependency detail, so a failure
// says which one is down without an operator having to read logs.
// Kubernetes should wire this to readinessProbe.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	type check struct {
		name    string
		healthy bool
		detail  string
	}
	checks := make([]check, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		c := &checks[0]
		c.name = "database"
		if err := s.store.Ping(ctx); err != nil {
			c.detail = err.Error()
			return
		}
		c.healthy = true
	}()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		c := &checks[1]
		c.name = "rpc"
		h, err := s.rpc.GetHealth(ctx)
		if err != nil {
			c.detail = err.Error()
			return
		}
		c.healthy = true
		c.detail = "latest ledger " + strconv.FormatInt(int64(h.LatestLedger), 10)
	}()
	wg.Wait()

	status := http.StatusOK
	byName := map[string]any{}
	for _, c := range checks {
		entry := map[string]any{"healthy": c.healthy}
		if c.detail != "" {
			entry["detail"] = c.detail
		}
		byName[c.name] = entry
		if !c.healthy {
			status = http.StatusServiceUnavailable
		}
	}
	out := map[string]any{"checks": byName}
	if status == http.StatusOK {
		out["status"] = "ready"
	} else {
		out["status"] = "not ready"
	}
	writeJSON(w, status, out)
}
