package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sorotrail/sorobeacon/internal/apierr"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

const (
	// IdempotencyKeyHeader is the request header clients send on write
	// endpoints (POST, PUT, PATCH, DELETE) to make retries safe.
	IdempotencyKeyHeader = "Idempotency-Key"

	// IdempotencyReplayedHeader is set to "true" when a response is served
	// from a previously completed idempotent operation rather than re-executed.
	IdempotencyReplayedHeader = "Idempotency-Replayed"

	idempotencyPollInterval = 15 * time.Millisecond
	idempotencyWaitTimeout  = 10 * time.Second
)

type inFlightIdempotency struct {
	method      string
	path        string
	requestHash string
	done        chan struct{}
	rec         *store.IdempotencyRecord
	aborted     bool
}

// WithIdempotencyTTL overrides the retention window for idempotency keys
// (default 24h). Non-positive durations are ignored.
func (s *Server) WithIdempotencyTTL(d time.Duration) *Server {
	if d > 0 {
		s.idempotencyTTL = d
	}
	return s
}

// WithIdempotencyClock overrides the clock consulted when claiming and
// expiring idempotency keys (used in tests).
func (s *Server) WithIdempotencyClock(now func() time.Time) *Server {
	if now != nil {
		s.idempotencyClock = now
	}
	return s
}

func (s *Server) idempotencyNow() time.Time {
	if s.idempotencyClock != nil {
		return s.idempotencyClock().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) effectiveIdempotencyTTL() time.Duration {
	if s.idempotencyTTL > 0 {
		return s.idempotencyTTL
	}
	return store.DefaultIdempotencyTTL
}

func canonicalRequestPath(r *http.Request) string {
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	if r.URL.RawQuery != "" {
		return p + "?" + r.URL.RawQuery
	}
	return p
}

func idempotencyScopeKey(ctx context.Context, key string) string {
	ws, ok := workspace.From(ctx)
	if !ok || ws == "" {
		ws = workspace.Default
	}
	return string(ws) + "\x00" + key
}

// idempotencyMiddleware deduplicates mutating HTTP requests (POST, PUT, PATCH,
// DELETE) that supply an Idempotency-Key header, scoped to (workspace_id, key).
func (s *Server) idempotencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mutatingMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		rawKey := strings.TrimSpace(r.Header.Get(IdempotencyKeyHeader))
		if rawKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(rawKey) > store.MaxIdempotencyKeyLength {
			writeErrCode(w, r, http.StatusBadRequest, apierr.CodeInvalidArgument,
				"Idempotency-Key must be at most 255 characters")
			return
		}

		var bodyBytes []byte
		if r.Body != nil {
			var err error
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					if m := metricsFrom(r.Context()); m != nil {
						m.RecordBodyRejection(rejectionLimitType(r))
					}
					writeErr(w, r, http.StatusRequestEntityTooLarge, "request body too large")
					return
				}
				writeErr(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		method := strings.ToUpper(r.Method)
		path := canonicalRequestPath(r)
		reqHash := store.HashIdempotentRequest(method, path, bodyBytes)
		now := s.idempotencyNow()
		expiresAt := now.Add(s.effectiveIdempotencyTTL())
		scopeKey := idempotencyScopeKey(r.Context(), rawKey)

		// Coordinate in-process concurrent requests before hitting the database so
		// parallel retries on the same replica coalesce deterministically without
		// SQLITE_BUSY contention.
		s.idempotencyMu.Lock()
		if flight, active := s.idempotencyFlight[scopeKey]; active {
			if flight.method != method || flight.path != path || flight.requestHash != reqHash {
				s.idempotencyMu.Unlock()
				writeIdempotencyConflict(w, r)
				return
			}
			s.idempotencyMu.Unlock()
			s.waitForInFlightIdempotency(w, r, flight, rawKey, method, path, reqHash)
			return
		}

		outcome, rec, supported, err := store.SafeClaimIdempotencyKey(
			r.Context(), s.store, rawKey, method, path, reqHash, now, expiresAt,
		)
		if !supported {
			outcome, rec, err = s.claimMemoryIdempotencyKeyLocked(
				scopeKey, rawKey, method, path, reqHash, now, expiresAt,
			)
		}
		if err != nil {
			s.idempotencyMu.Unlock()
			if errors.Is(err, store.ErrIdempotencyKeyConflict) {
				writeIdempotencyConflict(w, r)
				return
			}
			s.fail(w, r, err)
			return
		}

		switch outcome {
		case store.IdempotencyReplayed:
			s.idempotencyMu.Unlock()
			replayIdempotencyResponse(w, rec)
			return

		case store.IdempotencyConflict:
			s.idempotencyMu.Unlock()
			writeIdempotencyConflict(w, r)
			return

		case store.IdempotencyInProgress:
			s.idempotencyMu.Unlock()
			s.pollRemoteInFlightIdempotency(w, r, rawKey, method, path, reqHash)
			return
		}

		flight := &inFlightIdempotency{
			method:      method,
			path:        path,
			requestHash: reqHash,
			done:        make(chan struct{}),
		}
		if s.idempotencyFlight == nil {
			s.idempotencyFlight = make(map[string]*inFlightIdempotency)
		}
		s.idempotencyFlight[scopeKey] = flight
		s.idempotencyMu.Unlock()

		recorder := &idempotencyResponseRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		panicked := true
		defer func() {
			s.idempotencyMu.Lock()
			delete(s.idempotencyFlight, scopeKey)
			if panicked || recorder.status >= http.StatusInternalServerError {
				if supported {
					_ = store.SafeDeleteIdempotencyKey(r.Context(), s.store, rawKey)
				} else {
					delete(s.idempotencyMem, scopeKey)
				}
				flight.aborted = true
			}
			close(flight.done)
			s.idempotencyMu.Unlock()
		}()

		next.ServeHTTP(recorder, r)
		panicked = false

		if recorder.status >= http.StatusInternalServerError {
			return
		}

		contentType := recorder.Header().Get("Content-Type")
		respBytes := append([]byte(nil), recorder.body.Bytes()...)
		completedRec := &store.IdempotencyRecord{
			IdempotencyKey: rawKey,
			Method:         method,
			Path:           path,
			RequestHash:    reqHash,
			StatusCode:     recorder.status,
			ContentType:    contentType,
			ResponseBody:   respBytes,
			Completed:      true,
			CreatedAt:      now,
			ExpiresAt:      expiresAt,
		}

		s.idempotencyMu.Lock()
		if supported {
			_ = store.SafeCompleteIdempotencyKey(
				r.Context(), s.store, rawKey, reqHash,
				recorder.status, contentType, respBytes, expiresAt,
			)
		} else {
			if s.idempotencyMem == nil {
				s.idempotencyMem = make(map[string]*store.IdempotencyRecord)
			}
			s.idempotencyMem[scopeKey] = completedRec
		}
		flight.rec = completedRec
		s.idempotencyMu.Unlock()
	})
}

func (s *Server) claimMemoryIdempotencyKeyLocked(
	scopeKey, key, method, path, reqHash string,
	now, expiresAt time.Time,
) (store.IdempotencyClaimOutcome, *store.IdempotencyRecord, error) {
	if s.idempotencyMem == nil {
		s.idempotencyMem = make(map[string]*store.IdempotencyRecord)
	}
	if existing, ok := s.idempotencyMem[scopeKey]; ok {
		if !existing.ExpiresAt.After(now) {
			delete(s.idempotencyMem, scopeKey)
		} else {
			if existing.Method != method || existing.Path != path || existing.RequestHash != reqHash {
				return store.IdempotencyConflict, existing, store.ErrIdempotencyKeyConflict
			}
			if existing.Completed {
				return store.IdempotencyReplayed, existing, nil
			}
			return store.IdempotencyInProgress, existing, nil
		}
	}
	rec := &store.IdempotencyRecord{
		IdempotencyKey: key,
		Method:         method,
		Path:           path,
		RequestHash:    reqHash,
		Completed:      false,
		CreatedAt:      now,
		ExpiresAt:      expiresAt,
	}
	s.idempotencyMem[scopeKey] = rec
	return store.IdempotencyClaimed, rec, nil
}

func (s *Server) waitForInFlightIdempotency(
	w http.ResponseWriter,
	r *http.Request,
	flight *inFlightIdempotency,
	key, method, path, reqHash string,
) {
	select {
	case <-flight.done:
		if flight.rec != nil {
			if flight.rec.Method != method || flight.rec.Path != path || flight.rec.RequestHash != reqHash {
				writeIdempotencyConflict(w, r)
				return
			}
			replayIdempotencyResponse(w, flight.rec)
			return
		}
		s.pollRemoteInFlightIdempotency(w, r, key, method, path, reqHash)
	case <-r.Context().Done():
		writeErrCode(w, r, http.StatusConflict, apierr.CodeConflict,
			"a request with this Idempotency-Key is currently in progress")
	}
}

func (s *Server) pollRemoteInFlightIdempotency(
	w http.ResponseWriter,
	r *http.Request,
	key, method, path, reqHash string,
) {
	deadline := time.Now().Add(idempotencyWaitTimeout)
	ticker := time.NewTicker(idempotencyPollInterval)
	defer ticker.Stop()

	for {
		rec, err := store.SafeGetIdempotencyKey(r.Context(), s.store, key, s.idempotencyNow())
		if err == nil && rec != nil {
			if rec.Method != method || rec.Path != path || rec.RequestHash != reqHash {
				writeIdempotencyConflict(w, r)
				return
			}
			if rec.Completed {
				replayIdempotencyResponse(w, rec)
				return
			}
		} else if errors.Is(err, store.ErrNotFound) {
			break
		}

		if time.Now().After(deadline) {
			break
		}
		select {
		case <-r.Context().Done():
			writeErrCode(w, r, http.StatusConflict, apierr.CodeConflict,
				"a request with this Idempotency-Key is currently in progress")
			return
		case <-ticker.C:
		}
	}

	writeErrCode(w, r, http.StatusConflict, apierr.CodeConflict,
		"a request with this Idempotency-Key is currently in progress")
}

func writeIdempotencyConflict(w http.ResponseWriter, r *http.Request) {
	writeErrCode(w, r, http.StatusConflict, apierr.CodeConflict,
		"Idempotency-Key was already used with a different request method, path, or body")
}

func replayIdempotencyResponse(w http.ResponseWriter, rec *store.IdempotencyRecord) {
	if rec.ContentType != "" {
		w.Header().Set("Content-Type", rec.ContentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(IdempotencyReplayedHeader, "true")
	w.WriteHeader(rec.StatusCode)
	if len(rec.ResponseBody) > 0 && rec.StatusCode != http.StatusNoContent {
		_, _ = w.Write(rec.ResponseBody)
	}
}

type idempotencyResponseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (r *idempotencyResponseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *idempotencyResponseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	_, _ = r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *idempotencyResponseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
